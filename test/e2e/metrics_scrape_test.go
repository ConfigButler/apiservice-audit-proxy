//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// TestProxyMetricsScrapeAfterWatch proves three PR #8 hardening behaviours
// on the wire, in a real cluster:
//
//  1. The /metrics endpoint is reachable via the chart's metrics Service port
//     (the apiserver's service-proxy is used to avoid bookkeeping a local
//     port-forward).
//  2. A Kubernetes-style chunked list response is NOT classified as a stream:
//     the requests_total counter carries streaming="false" for verb=list.
//  3. A real watch request IS classified as a stream: the requests_total
//     counter carries streaming="true" for verb=watch, and the
//     streams_active gauge plus stream_duration_seconds histogram exercise
//     the lifecycle (active>0 during the watch, settles back to 0 after).
//
// Items (2) and (3) together prove the Section 2 narrowing: chunked transfer
// encoding alone no longer dribbles into stream metrics.
func TestProxyMetricsScrapeAfterWatch(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)

	// Step 1: trigger a list (chunked from the aggregated API). This is the
	// case that was being mislabeled as streaming under the old
	// ContentLength==-1 predicate.
	client.run(ctx, "get", "flunders", "-n", "default")

	// Step 2: trigger a short watch so the streaming positive case is
	// exercised; the goroutine is unblocked when we cancel watchCtx.
	flunderName := fmt.Sprintf("metrics-watch-%d", time.Now().UTC().Unix())
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	watchCtx, cancelWatch := context.WithTimeout(ctx, 30*time.Second)
	defer cancelWatch()
	watchCmd := client.command(watchCtx,
		"get", "flunders", "-n", "default", "--watch", "--no-headers")
	stdoutPipe, err := watchCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("watch stdout pipe: %v", err)
	}
	if err := watchCmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}

	seenWatchEvent := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), flunderName) {
				select {
				case seenWatchEvent <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// Give the watch a moment to be established before creating the object
	// so the ADDED event arrives over the open stream.
	time.Sleep(2 * time.Second)
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: metrics-scrape
`, flunderName))

	select {
	case <-seenWatchEvent:
	case <-time.After(15 * time.Second):
		t.Fatalf("watch did not receive event for %s", flunderName)
	}

	// While the watch is still open, the active gauge must include ours.
	// A real cluster may have other in-flight watches (kube-controller-manager
	// etc.) so we anchor on a delta around our scenario rather than an
	// absolute value.
	metricsDuring := scrapeProxyMetrics(t, ctx, client)
	gaugeDuring := metricsDuring.gauge("apiservice_audit_proxy_streams_active",
		map[string]string{"kind": "watch"})
	if gaugeDuring < 1 {
		t.Fatalf("streams_active{kind=watch} during open watch = %d, want >= 1\n\n%s",
			gaugeDuring, metricsDuring.dump("apiservice_audit_proxy_streams_active"))
	}
	durationCountDuring := metricsDuring.histogramCount(
		"apiservice_audit_proxy_stream_duration_seconds",
		map[string]string{"kind": "watch"})

	// Close the watch and let the stream lifecycle settle.
	cancelWatch()
	_ = watchCmd.Wait()

	// Step 3: scrape /metrics and assert the hardening invariants. We poll
	// until the proxy has observed our stream closing, evidenced by the
	// histogram count increasing — that's a per-scenario delta and avoids
	// brittle absolute checks against unrelated cluster watches.
	var metricsAfter promFamilies
	waitFor(t, 20*time.Second, func() error {
		metricsAfter = scrapeProxyMetrics(t, ctx, client)
		after := metricsAfter.histogramCount(
			"apiservice_audit_proxy_stream_duration_seconds",
			map[string]string{"kind": "watch"})
		if after <= durationCountDuring {
			return fmt.Errorf(
				"stream_duration_seconds{kind=watch}_count did not advance after closing our watch (before=%d after=%d)",
				durationCountDuring, after)
		}
		return nil
	})

	// (2) chunked list response must carry streaming="false".
	if !metricsAfter.hasCounter("apiservice_audit_proxy_requests_total",
		map[string]string{"verb": "list", "resource": "flunders", "streaming": "false"}) {
		t.Fatalf("expected requests_total{verb=list,resource=flunders,streaming=\"false\"} > 0 — "+
			"chunked list response must NOT be classified as streaming\n\n%s",
			metricsAfter.dump("apiservice_audit_proxy_requests_total"))
	}
	if metricsAfter.hasCounter("apiservice_audit_proxy_requests_total",
		map[string]string{"verb": "list", "resource": "flunders", "streaming": "true"}) {
		t.Fatalf("requests_total{verb=list,resource=flunders,streaming=\"true\"} unexpectedly present — "+
			"narrowed stream classification regressed\n\n%s",
			metricsAfter.dump("apiservice_audit_proxy_requests_total"))
	}

	// (3) the watch must carry streaming="true".
	if !metricsAfter.hasCounter("apiservice_audit_proxy_requests_total",
		map[string]string{"verb": "watch", "resource": "flunders", "streaming": "true"}) {
		t.Fatalf("expected requests_total{verb=watch,streaming=\"true\"} > 0\n\n%s",
			metricsAfter.dump("apiservice_audit_proxy_requests_total"))
	}

	// (the stream_duration_seconds delta is asserted above via waitFor)

	// Transport bytes flowed on the backend leg for streaming reads.
	if metricsAfter.counter("apiservice_audit_proxy_transport_bytes_total",
		map[string]string{"leg": "backend", "streaming": "true", "direction": "read"}) <= 0 {
		t.Fatalf("expected transport_bytes_total{leg=backend,streaming=true,direction=read} > 0\n\n%s",
			metricsAfter.dump("apiservice_audit_proxy_transport_bytes_total"))
	}
}

// scrapeProxyMetrics hits /metrics through the apiserver service-proxy so the
// test does not need a local port-forward to the proxy's metrics port.
func scrapeProxyMetrics(t *testing.T, ctx context.Context, client kubectlClient) promFamilies {
	t.Helper()
	namespace := envOrDefault("E2E_PROXY_NAMESPACE", "wardle")
	release := envOrDefault("E2E_RELEASE_NAME", "apiservice-audit-proxy")
	raw := client.run(ctx,
		"get", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/services/%s:metrics/proxy/metrics",
			namespace, release),
	)
	return parsePromText(t, raw)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// promFamilies wraps expfmt's parsed output with the small set of
// label-aware accessors this test needs.
type promFamilies struct {
	raw      string
	families map[string]*dto.MetricFamily
}

func parsePromText(t *testing.T, text string) promFamilies {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse /metrics response: %v\n\n%s", err, text)
	}
	return promFamilies{raw: text, families: families}
}

func (p promFamilies) matchAll(name string, match map[string]string) []*dto.Metric {
	family := p.families[name]
	if family == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range family.GetMetric() {
		if labelsMatch(m.GetLabel(), match) {
			out = append(out, m)
		}
	}
	return out
}

func labelsMatch(have []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		if findLabel(have, k) != v {
			return false
		}
	}
	return true
}

func findLabel(pairs []*dto.LabelPair, name string) string {
	for _, p := range pairs {
		if p.GetName() == name {
			return p.GetValue()
		}
	}
	return ""
}

func (p promFamilies) hasCounter(name string, match map[string]string) bool {
	for _, m := range p.matchAll(name, match) {
		if m.GetCounter().GetValue() > 0 {
			return true
		}
	}
	return false
}

func (p promFamilies) counter(name string, match map[string]string) float64 {
	var sum float64
	for _, m := range p.matchAll(name, match) {
		sum += m.GetCounter().GetValue()
	}
	return sum
}

func (p promFamilies) gauge(name string, match map[string]string) int64 {
	var sum int64
	for _, m := range p.matchAll(name, match) {
		sum += int64(m.GetGauge().GetValue())
	}
	return sum
}

func (p promFamilies) histogramCount(name string, match map[string]string) int64 {
	var total int64
	for _, m := range p.matchAll(name, match) {
		total += int64(m.GetHistogram().GetSampleCount())
	}
	return total
}

func (p promFamilies) dump(name string) string {
	family := p.families[name]
	if family == nil {
		return "  (no samples for " + name + ")"
	}
	var lines []string
	for _, m := range family.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		var value string
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			value = fmt.Sprintf("%g", m.GetCounter().GetValue())
		case dto.MetricType_GAUGE:
			value = fmt.Sprintf("%g", m.GetGauge().GetValue())
		case dto.MetricType_HISTOGRAM:
			h := m.GetHistogram()
			value = fmt.Sprintf("count=%d sum=%g", h.GetSampleCount(), h.GetSampleSum())
		default:
			value = "<unsupported>"
		}
		lines = append(lines, fmt.Sprintf("  %s%v = %s", name, labels, value))
	}
	return strings.Join(lines, "\n")
}
