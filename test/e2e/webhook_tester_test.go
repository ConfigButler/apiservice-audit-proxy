//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const (
	// Session UUIDs match test/e2e/cluster/audit/webhook-config.yaml and
	// charts/apiservice-audit-proxy/values.yaml. Fixed values keep browser URLs
	// and the kube-apiserver audit webhook stable across cluster recreations.
	auditGapKubeApiserverSessionUUID = "aabbccdd-0000-4000-0000-000000000001"
	auditGapProxySessionUUID         = "aabbccdd-0000-4000-0000-000000000002"

	webhookTesterSvcName   = "apiservice-audit-proxy-webhook-tester"
	webhookTesterLocalPort = "18090"
	webhookTesterSvcPort   = "8080"
)

type webhookTesterEntry struct {
	RequestPayloadBase64 string `json:"request_payload_base64"`
}

// fetchAuditEvents queries the webhook-tester REST API for all captured
// requests in the given session, base64-decodes each body, unmarshals the
// audit.k8s.io/v1 EventList, and returns a flat slice of events. Returns nil
// when the session does not yet exist (HTTP 404).
func fetchAuditEvents(t *testing.T, baseURL, sessionUUID string) []auditv1.Event {
	t.Helper()

	resp, err := http.Get(baseURL + "/api/session/" + sessionUUID + "/requests")
	if err != nil {
		t.Logf("fetch session %s: %v", sessionUUID, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		t.Logf("fetch session %s: unexpected status %d", sessionUUID, resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read session %s response: %v", sessionUUID, err)
	}

	var entries []webhookTesterEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("decode session %s entries: %v\n%s", sessionUUID, err, string(body))
	}

	var events []auditv1.Event
	for _, entry := range entries {
		raw, err := base64.StdEncoding.DecodeString(entry.RequestPayloadBase64)
		if err != nil {
			t.Logf("base64-decode payload in session %s: %v (skipping)", sessionUUID, err)
			continue
		}
		var list auditv1.EventList
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Logf("decode EventList in session %s: %v (skipping)", sessionUUID, err)
			continue
		}
		events = append(events, list.Items...)
	}
	return events
}

// clearWebhookTesterSession deletes a webhook-tester session so prior test
// runs do not pollute the event stream. A 404 (session never existed) is fine.
func clearWebhookTesterSession(t *testing.T, baseURL, sessionUUID string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete,
		baseURL+"/api/session/"+sessionUUID, nil)
	if err != nil {
		t.Fatalf("build DELETE request for session %s: %v", sessionUUID, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("clear session %s: %v (ignored)", sessionUUID, err)
		return
	}
	_ = resp.Body.Close()
}

// startPortForwardToServicePort starts a kubectl port-forward to a specific
// service port and returns the base URL and a stop function.
func startPortForwardToServicePort(
	t *testing.T,
	ctx context.Context,
	client kubectlClient,
	namespace, serviceName, localPort, servicePort string,
) (string, func()) {
	t.Helper()

	pfCtx, cancel := context.WithCancel(ctx)
	cmd := client.command(pfCtx,
		"-n", namespace, "port-forward",
		"svc/"+serviceName,
		localPort+":"+servicePort,
	)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start kubectl port-forward to %s/%s: %v", namespace, serviceName, err)
	}
	return "http://127.0.0.1:" + localPort, func() { cancel(); _ = cmd.Wait() }
}
