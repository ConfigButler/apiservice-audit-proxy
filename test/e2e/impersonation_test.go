//go:build e2e

package e2e

// Impersonation-mode end-to-end scenarios.
//
// These exercise the proxy running with backend.identity.mode=impersonation:
// the proxy authenticates the inbound delegated identity, then calls the
// backend as its own ServiceAccount with Impersonate-* headers.
//
// TestImpersonationWrite, TestImpersonationPassthrough, and
// TestImpersonationApiserverExtras run against test/e2e/values/proxy-impersonation.yaml
// (operator-supplied impersonation RBAC). TestImpersonationRBACMissing runs
// against test/e2e/values/proxy-impersonation-no-rbac.yaml.
//
// All four require a live k3d cluster installed by the e2e harness; run them
// via the e2e:test-impersonation / e2e:test-impersonation-no-rbac Taskfile
// tasks. Environment: CTX (kube context), WEBHOOK_TESTER_BASE_URL.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// proxyServiceAccountUser is the ServiceAccount the proxy runs as. The audited
// event must never record this identity — it must record the real delegated
// user. The namespace matches E2E_PROXY_NAMESPACE (default "wardle") and the
// name matches the Helm release name.
const proxyServiceAccountUser = "system:serviceaccount:wardle:apiservice-audit-proxy"

func waitForWardleAPIService(t *testing.T, client kubectlClient) {
	t.Helper()
	client.run(context.Background(),
		"wait", "apiservice/v1alpha1.wardle.example.com",
		`--for=jsonpath={.status.conditions[?(@.type=="Available")].status}=True`,
		"--timeout=240s",
	)
}

// requireImpersonationMode fails the test unless the proxy Deployment is
// actually running in impersonation mode. Without this guard the impersonation
// scenarios would silently false-pass against a requestheader-mode deploy,
// because the e2e backend also trusts the forwarded requestheader identity.
func requireImpersonationMode(t *testing.T, client kubectlClient) {
	t.Helper()
	args := client.run(context.Background(),
		"-n", "wardle", "get", "deploy/apiservice-audit-proxy",
		"-o", "jsonpath={.spec.template.spec.containers[0].args}")
	if !strings.Contains(args, "--backend-identity-mode=impersonation") {
		t.Fatalf("proxy is not deployed in impersonation mode; deployment args: %s", args)
	}
}

func waitForWebhookTester(t *testing.T, wtURL string) {
	t.Helper()
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
}

// TestImpersonationWrite — scenario 1: an audited write succeeds and the proxy
// emits a complete audit event whose user is the real delegated kubectl
// identity, never the proxy ServiceAccount.
func TestImpersonationWrite(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)
	requireImpersonationMode(t, client)

	wtURL := webhookTesterBaseURL()
	waitForWebhookTester(t, wtURL)
	clearWebhookTesterSession(t, wtURL, auditGapProxySessionUUID)

	flunderName := fmt.Sprintf("imp-write-%d", time.Now().UTC().Unix())
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: impersonation-write
`, flunderName))
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	// The create itself must succeed end-to-end.
	client.run(ctx, "get", "flunder", flunderName, "-o", "json")

	waitFor(t, 120*time.Second, func() error {
		events := fetchAuditEvents(t, wtURL, auditGapProxySessionUUID)
		for i := range events {
			ev := &events[i]
			if ev.ObjectRef == nil || ev.ObjectRef.Name != flunderName {
				continue
			}
			if ev.User.Username == "" {
				return fmt.Errorf("event for %s has an empty user", flunderName)
			}
			if ev.User.Username == proxyServiceAccountUser {
				t.Fatalf("audit event recorded the proxy ServiceAccount %q instead of the delegated user",
					ev.User.Username)
			}
			if strings.HasPrefix(ev.User.Username, "system:serviceaccount:wardle:apiservice-audit-proxy") {
				t.Fatalf("audit event user %q is the proxy ServiceAccount", ev.User.Username)
			}
			if ev.RequestObject == nil || ev.ResponseObject == nil {
				return fmt.Errorf("event for %s missing RequestObject or ResponseObject", flunderName)
			}
			t.Logf("impersonation write audit event: user=%q", ev.User.Username)
			return nil
		}
		return fmt.Errorf("waiting for complete audit event for flunder %s", flunderName)
	})
}

// TestImpersonationPassthrough — scenario 2: non-audited GET (list) and watch
// requests succeed, proving the ReverseProxy Rewrite hook applies the same
// impersonation identity. If it did not, the backend would authorize the bare
// proxy ServiceAccount and reject the request.
func TestImpersonationPassthrough(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)
	requireImpersonationMode(t, client)

	// list — a non-audited GET through the ReverseProxy passthrough path.
	client.run(ctx, "get", "flunders", "-n", "default")

	// watch — start a watch, then create a flunder and assert the watch streams
	// it, proving streaming survives the impersonation Rewrite hook.
	flunderName := fmt.Sprintf("imp-watch-%d", time.Now().UTC().Unix())
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	watchCtx, cancelWatch := context.WithTimeout(ctx, 40*time.Second)
	defer cancelWatch()

	// Stream the watch output line by line — kubectl --watch does not exit on
	// its own, so the flunder must be detected as it is streamed rather than
	// after the command terminates.
	watchCmd := client.command(watchCtx, "get", "flunders", "-n", "default", "--watch", "--no-headers")
	stdout, err := watchCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("watch stdout pipe: %v", err)
	}
	if err := watchCmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}
	t.Cleanup(func() { _ = watchCmd.Wait() })

	seen := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), flunderName) {
				select {
				case seen <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// Give the watch a moment to establish, then create.
	time.Sleep(3 * time.Second)
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: impersonation-watch
`, flunderName))

	select {
	case <-seen:
		// The watch streamed the concurrently created flunder, proving streaming
		// survives the impersonation Rewrite hook.
	case <-time.After(25 * time.Second):
		t.Fatalf("watch did not stream flunder %s", flunderName)
	}
	cancelWatch()
}

// TestImpersonationApiserverExtras — scenario 4: with extras.mode=none a write
// still succeeds, even though a normal kubectl request carries
// apiserver-injected extras such as
// authentication.kubernetes.io/credential-id. Success proves the proxy dropped
// the un-allowlisted extras rather than requiring userextras RBAC for them.
func TestImpersonationApiserverExtras(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)
	requireImpersonationMode(t, client)

	flunderName := fmt.Sprintf("imp-extras-%d", time.Now().UTC().Unix())
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: impersonation-extras
`, flunderName))

	// A successful read-back confirms the create was authorized despite the
	// apiserver-injected extras the proxy deliberately dropped.
	client.run(ctx, "get", "flunder", flunderName, "-o", "json")
}

// TestImpersonationRBACMissing — scenario 3: with no impersonation RBAC the
// backend rejects the proxy ServiceAccount's impersonated requests with a clean
// 403, the failure is observable, and the proxy itself stays healthy.
//
// Note on the observed signal: the kube-apiserver aggregation-layer discovery
// health check is itself a proxied request, so without RBAC it fails too and
// the APIService is reported Unavailable (reason FailedDiscoveryCheck). A
// subsequent client write is then short-circuited by the kube-apiserver with
// ServiceUnavailable before it is ever proxied — so the literal backend body
// text "cannot impersonate" does not reach the client. The kube-apiserver
// records the backend's 403 status code in the APIService condition, which is
// the reliable observable signal that the failure is a clean authorization
// rejection (403) proxied straight through, not a proxy 500 or crash.
func TestImpersonationRBACMissing(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	requireImpersonationMode(t, client)

	// The backend's 403 surfaces in the APIService Available condition.
	waitFor(t, 180*time.Second, func() error {
		status := client.run(ctx, "get", "apiservice/v1alpha1.wardle.example.com",
			"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`)
		reason := client.run(ctx, "get", "apiservice/v1alpha1.wardle.example.com",
			"-o", `jsonpath={.status.conditions[?(@.type=="Available")].reason}`)
		message := client.run(ctx, "get", "apiservice/v1alpha1.wardle.example.com",
			"-o", `jsonpath={.status.conditions[?(@.type=="Available")].message}`)
		if status != "False" {
			return fmt.Errorf("APIService Available=%q, waiting for False", status)
		}
		if reason != "FailedDiscoveryCheck" || !strings.Contains(message, "403") {
			return fmt.Errorf("expected a 403 FailedDiscoveryCheck, got reason=%q message=%q", reason, message)
		}
		t.Logf("missing-RBAC: backend rejected impersonation with 403 — %s", message)
		return nil
	})

	// The write itself must fail, not succeed.
	flunderName := fmt.Sprintf("imp-norbac-%d", time.Now().UTC().Unix())
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	manifest := fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: impersonation-no-rbac
`, flunderName)

	cmd := client.command(ctx, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected the create to fail without impersonation RBAC, got success:\n%s", combined.String())
	}
	t.Logf("missing-RBAC create failed as expected: %s", strings.TrimSpace(combined.String()))

	// The proxy must stay healthy — the rejection is the backend's, and the
	// proxy must not crash or report a 500.
	client.run(ctx, "-n", "wardle", "rollout", "status",
		"deploy/apiservice-audit-proxy", "--timeout=30s")
}
