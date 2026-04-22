//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	smokeNamespace = "audit-pass-through-smoke"
	webhookPort    = "19444"
)

func TestSmoke(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	webhookNamespace := requireEnv(t, "WEBHOOK_NAMESPACE")
	webhookServiceName := requireEnv(t, "WEBHOOK_SERVICE_NAME")

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

	webhookURL, stopPortForward := client.startPortForward(ctx, webhookNamespace, webhookServiceName, webhookPort)
	defer stopPortForward()

	waitFor(t, 60*time.Second, func() error {
		response, err := http.Get(webhookURL + "/events")
		if err != nil {
			return err
		}
		defer func() {
			_ = response.Body.Close()
		}()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected /events status: %d", response.StatusCode)
		}
		return nil
	})

	var payload eventsPayload
	waitFor(t, 180*time.Second, func() error {
		response, err := http.Get(webhookURL + "/events")
		if err != nil {
			return err
		}
		defer func() {
			_ = response.Body.Close()
		}()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected /events status: %d", response.StatusCode)
		}

		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		decodeJSON(t, body, &payload)

		for _, item := range payload.Items {
			for _, event := range item.EventList.Items {
				if event.ObjectRef.Name != flunderName {
					continue
				}
				if event.RequestObject == nil || event.ResponseObject == nil {
					return fmt.Errorf("event for %s does not yet contain requestObject and responseObject", flunderName)
				}
				return nil
			}
		}

		return fmt.Errorf("waiting for recovered audit payload for %s", flunderName)
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

func (c kubectlClient) startPortForward(ctx context.Context, namespace, serviceName, localPort string) (string, func()) {
	c.t.Helper()

	portForwardCtx, cancel := context.WithCancel(ctx)
	logPath := filepath.Join(c.t.TempDir(), "port-forward.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		c.t.Fatalf("create port-forward log file: %v", err)
	}

	cmd := c.command(portForwardCtx, "-n", namespace, "port-forward", "svc/"+serviceName, localPort+":9444")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		c.t.Fatalf("start kubectl port-forward: %v", err)
	}

	stop := func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
	}

	return "http://127.0.0.1:" + localPort, stop
}

type eventsPayload struct {
	Items []struct {
		EventList struct {
			Items []struct {
				ObjectRef struct {
					Name string `json:"name"`
				} `json:"objectRef"`
				RequestObject  map[string]any `json:"requestObject"`
				ResponseObject map[string]any `json:"responseObject"`
			} `json:"items"`
		} `json:"eventList"`
	} `json:"items"`
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
