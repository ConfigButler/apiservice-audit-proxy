//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	smokeNamespace = "audit-pass-through-smoke"
)

func TestSmoke(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")

	client := newKubectlClient(t, kubectlContext)

	client.run(ctx,
		"wait",
		"apiservice/v1alpha1.wardle.example.com",
		`--for=jsonpath={.status.conditions[?(@.type=="Available")].status}=True`,
		"--timeout=240s",
	)
	client.run(ctx, "api-resources", "--api-group=wardle.example.com")
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, smokeNamespace))

	flunderName := fmt.Sprintf("smoke-%d", time.Now().UTC().Unix())
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: smoke-reference
`, flunderName, smokeNamespace))
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"-n", smokeNamespace, "delete", "flunder", flunderName,
			"--ignore-not-found", "--wait=false").Run()
	})

	flunderJSON := client.run(ctx, "-n", smokeNamespace, "get", "flunder", flunderName, "-o", "json")
	var flunder struct {
		APIVersion string `json:"apiVersion"`
		Spec       struct {
			Reference string `json:"reference"`
		} `json:"spec"`
	}
	decodeJSON(t, []byte(flunderJSON), &flunder)
	if flunder.APIVersion != "wardle.example.com/v1alpha1" || flunder.Spec.Reference != "smoke-reference" {
		t.Fatalf("unexpected flunder payload: %s", flunderJSON)
	}

	wtURL := webhookTesterBaseURL()

	// Wait for webhook-tester to be reachable.
	waitFor(t, 30*time.Second, func() error {
		resp, err := http.Get(wtURL + "/healthz")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("webhook-tester /healthz: %d", resp.StatusCode)
		}
		return nil
	})

	// Poll the proxy session until the complete audit event for our flunder arrives.
	waitFor(t, 180*time.Second, func() error {
		events := fetchAuditEvents(t, wtURL, auditGapProxySessionUUID)
		for i := range events {
			if events[i].ObjectRef == nil || events[i].ObjectRef.Name != flunderName {
				continue
			}
			if events[i].RequestObject == nil || events[i].ResponseObject == nil {
				return fmt.Errorf("event for %s missing RequestObject or ResponseObject", flunderName)
			}
			return nil
		}
		return fmt.Errorf("waiting for complete audit event for flunder %s", flunderName)
	})
}

type kubectlClient struct {
	t       *testing.T
	kubectl string
	context string
}

func newKubectlClient(t *testing.T, contextName string) kubectlClient {
	t.Helper()

	kubectlBinary := os.Getenv("KUBECTL")
	if kubectlBinary == "" {
		kubectlBinary = "kubectl"
	}

	return kubectlClient{
		t:       t,
		kubectl: kubectlBinary,
		context: contextName,
	}
}

func (c kubectlClient) command(ctx context.Context, args ...string) *exec.Cmd {
	commandArgs := append([]string{"--context", c.context}, args...)
	return exec.CommandContext(ctx, c.kubectl, commandArgs...)
}

func (c kubectlClient) run(ctx context.Context, args ...string) string {
	c.t.Helper()

	cmd := c.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		c.t.Fatalf("%s %s failed: %v\n%s\n%s", c.kubectl, strings.Join(args, " "), err, string(output), stderr.String())
	}

	return string(output)
}

func (c kubectlClient) applyYAML(ctx context.Context, manifest string) {
	c.t.Helper()

	cmd := c.command(ctx, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(strings.TrimSpace(manifest) + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("%s apply failed: %v\n%s", c.kubectl, err, string(output))
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()

	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required", key)
	}

	return value
}

func waitFor(t *testing.T, timeout time.Duration, condition func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := condition(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("timed out after %s: %v", timeout, lastErr)
}

func decodeJSON(t *testing.T, payload []byte, target any) {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode json: %v\n%s", err, string(payload))
	}
}
