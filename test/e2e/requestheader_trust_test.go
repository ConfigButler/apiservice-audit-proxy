//go:build e2e

package e2e

// Cluster-sourced requestheader trust end-to-end scenario.
//
// The proxy no longer takes inbound trust from a mounted Secret or
// --client-* flags; it reads the front-proxy CA, allowed client names, and
// identity header names live from kube-system/extension-apiserver-authentication.
//
// TestRequestHeaderTrustFromCluster asserts the three properties the design
// requires: no CA Secret is installed, an audited write still succeeds, and
// deleting the kube-system RoleBinding fails the proxy's startup rather than
// silently disabling verification.
//
// Requires a live k3d cluster installed by the e2e harness; run it via the
// e2e:test-requestheader-trust Taskfile task. Environment: CTX (kube context).

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	requestHeaderTrustNamespace   = "wardle"
	requestHeaderTrustRelease     = "apiservice-audit-proxy"
	requestHeaderTrustRoleBinding = "apiservice-audit-proxy-auth-reader"
)

func TestRequestHeaderTrustFromCluster(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)

	// (1) The proxy Deployment must consume no requestheader CA Secret or flag:
	// inbound trust is sourced live from the cluster ConfigMap. Inspect the whole
	// pod spec so a stray arg, volume, or mount is all caught.
	podSpec := client.run(ctx, "-n", requestHeaderTrustNamespace,
		"get", "deploy/"+requestHeaderTrustRelease, "-o", "jsonpath={.spec.template.spec}")
	for _, stale := range []string{"client-ca-file", "client-allowed-names", "requestheader-client-ca"} {
		if strings.Contains(podSpec, stale) {
			t.Fatalf("proxy Deployment still references %q; inbound trust must be cluster-sourced", stale)
		}
	}

	// (2) The kube-system auth-reader RoleBinding the chart adds must be present.
	client.run(ctx, "-n", "kube-system", "get", "rolebinding", requestHeaderTrustRoleBinding)

	// (3) An audited write succeeds end-to-end, proving the proxy verifies the
	// inbound front-proxy identity against the cluster-sourced CA.
	flunderName := fmt.Sprintf("rh-trust-%d", time.Now().UTC().Unix())
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
  reference: requestheader-trust
`, flunderName))
	client.run(ctx, "get", "flunder", flunderName, "-o", "json")

	// (4) Deleting the RoleBinding must fail the proxy's startup.
	assertStartupFailsWithoutRoleBinding(ctx, t, client, kubectlContext)
}

// assertStartupFailsWithoutRoleBinding deletes the kube-system auth-reader
// RoleBinding, forces a fresh rollout, and asserts the new pod never becomes
// ready: with no readable trust ConfigMap the proxy must fail its strict
// startup gate, not fall back to an unverified mode. It always restores the
// RoleBinding and a healthy rollout afterwards.
func assertStartupFailsWithoutRoleBinding(
	ctx context.Context,
	t *testing.T,
	client kubectlClient,
	kubectlContext string,
) {
	t.Helper()

	roleBindingManifest := fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: extension-apiserver-authentication-reader
subjects:
  - kind: ServiceAccount
    name: %s
    namespace: %s
`, requestHeaderTrustRoleBinding, requestHeaderTrustRelease, requestHeaderTrustNamespace)

	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		client.applyYAML(ctx, roleBindingManifest)
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"-n", requestHeaderTrustNamespace, "rollout", "restart",
			"deploy/"+requestHeaderTrustRelease).Run()
		client.run(ctx, "-n", requestHeaderTrustNamespace, "rollout", "status",
			"deploy/"+requestHeaderTrustRelease, "--timeout=180s")
	}
	t.Cleanup(restore)

	client.run(ctx, "-n", "kube-system", "delete", "rolebinding", requestHeaderTrustRoleBinding)
	client.run(ctx, "-n", requestHeaderTrustNamespace, "rollout", "restart",
		"deploy/"+requestHeaderTrustRelease)

	// The new pod cannot read the trust ConfigMap (Forbidden), so it must fail
	// its strict startup gate and never become ready. rollout status must time
	// out rather than succeed.
	cmd := client.command(ctx, "-n", requestHeaderTrustNamespace, "rollout", "status",
		"deploy/"+requestHeaderTrustRelease, "--timeout=90s")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err == nil {
		t.Fatalf("proxy rollout succeeded without the auth-reader RoleBinding; "+
			"startup must fail closed:\n%s", out.String())
	}
	t.Logf("proxy startup failed without the RoleBinding, as required: %s",
		strings.TrimSpace(out.String()))

	restore()
}
