//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyDeployment = "apiservice-audit-proxy"
	proxySelector   = "app.kubernetes.io/name=apiservice-audit-proxy"
)

func proxyTestNamespace() string {
	if ns := os.Getenv("E2E_PROXY_NAMESPACE"); ns != "" {
		return ns
	}
	return "wardle"
}

// TestImageRefreshNoopIsIdempotent verifies that task e2e:load-image is a no-op
// when neither the source nor the image has changed: the proxy pod must not restart.
func TestImageRefreshNoopIsIdempotent(t *testing.T) {
	kubectlCtx := requireEnv(t, "CTX")
	ns := proxyTestNamespace()
	projectDir := findProjectRoot(t)

	before := currentProxyPodName(t, kubectlCtx, ns)
	runTask(t, projectDir, "e2e:load-image")
	after := currentProxyPodName(t, kubectlCtx, ns)

	if before != after {
		t.Fatalf("pod restarted after no-op e2e:load-image: was %s, now %s", before, after)
	}
}

// TestImageRefreshTestFileDoesNotTriggerRebuild verifies that modifying a file
// under test/ does not bust the Docker build cache. The Dockerfile copies only
// cmd/ and pkg/, so test-only changes must leave the stamp digest unchanged.
//
// Skipped when E2E_PROXY_IMAGE is externally provided (CI path): the status
// guard on e2e:build-image skips rebuilds for non-default images, so the
// Docker layer cache behaviour is not exercised.
func TestImageRefreshTestFileDoesNotTriggerRebuild(t *testing.T) {
	if img := os.Getenv("E2E_PROXY_IMAGE"); img != "" && img != "apiservice-audit-proxy:e2e-local" {
		t.Skipf(
			"E2E_PROXY_IMAGE=%q is externally provided; rebuild chain tests only apply to local dev",
			img,
		)
	}

	kubectlCtx := requireEnv(t, "CTX")
	ns := proxyTestNamespace()
	projectDir := findProjectRoot(t)

	before := currentProxyPodName(t, kubectlCtx, ns)
	digestBefore := proxyStampDigest(t, projectDir, kubectlCtx)

	testFile := filepath.Join(projectDir, "test", "e2e", "smoke_test.go")
	original, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(testFile, original, 0600) })
	if err := os.WriteFile(
		testFile,
		append(original, []byte("\n// image-refresh-test\n")...),
		0600,
	); err != nil {
		t.Fatalf("modify test file: %v", err)
	}

	runTask(t, projectDir, "e2e:load-image")

	digestAfter := proxyStampDigest(t, projectDir, kubectlCtx)
	if digestBefore != digestAfter {
		t.Fatalf(
			"stamp changed after test-only file modification: was %s, now %s"+
				" (image was unexpectedly rebuilt)",
			digestBefore, digestAfter,
		)
	}

	after := currentProxyPodName(t, kubectlCtx, ns)
	if before != after {
		t.Fatalf(
			"pod restarted after test-only file modification: was %s, now %s",
			before, after,
		)
	}
}

// TestImageRefreshSourceChangeTriggersPodWithMatchingDigest verifies the full
// rebuild chain: a Go source change causes e2e:load-image to produce a new
// image, and after a rollout restart the running pod's containerStatus.imageID
// matches the digest recorded in the stamp file.
//
// Skipped when E2E_PROXY_IMAGE is externally provided (CI path).
func TestImageRefreshSourceChangeTriggersPodWithMatchingDigest(t *testing.T) {
	if img := os.Getenv("E2E_PROXY_IMAGE"); img != "" && img != "apiservice-audit-proxy:e2e-local" {
		t.Skipf(
			"E2E_PROXY_IMAGE=%q is externally provided; rebuild chain tests only apply to local dev",
			img,
		)
	}

	kubectlCtx := requireEnv(t, "CTX")
	ns := proxyTestNamespace()
	projectDir := findProjectRoot(t)

	before := currentProxyPodName(t, kubectlCtx, ns)

	srcFile := filepath.Join(projectDir, "cmd", "server", "main.go")
	original, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source file: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(srcFile, original, 0600) })
	if err := os.WriteFile(
		srcFile,
		append(original, []byte("\n// image-refresh-test\n")...),
		0600,
	); err != nil {
		t.Fatalf("modify source file: %v", err)
	}

	runTask(t, projectDir, "e2e:load-image")
	proxyRolloutRestart(t, kubectlCtx, ns)
	proxyRolloutWait(t, kubectlCtx, ns, 180*time.Second)

	after := currentProxyPodName(t, kubectlCtx, ns)
	if before == after {
		t.Fatalf("pod name unchanged after rollout restart (expected new pod): %s", after)
	}

	imageID := proxyPodImageID(t, kubectlCtx, ns, after)
	digest := proxyStampDigest(t, projectDir, kubectlCtx)
	if !strings.Contains(imageID, digest) {
		t.Fatalf("pod imageID %q does not contain stamp digest %q", imageID, digest)
	}
}

// --- helpers ---

func currentProxyPodName(t *testing.T, kubectlCtx, namespace string) string {
	t.Helper()
	out, err := exec.Command(
		"kubectl", "--context", kubectlCtx,
		"-n", namespace, "get", "pods",
		"-l", proxySelector,
		"-o", "json",
	).Output()
	if err != nil {
		t.Fatalf("get proxy pod name: %v", err)
	}

	var podList struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				CreationTimestamp string  `json:"creationTimestamp"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &podList); err != nil {
		t.Fatalf("parse proxy pod list: %v", err)
	}

	var newestName string
	var newestCreated time.Time
	for _, pod := range podList.Items {
		if pod.Status.Phase != "Running" || pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		if err != nil {
			t.Fatalf("parse creation timestamp for pod %s: %v", pod.Metadata.Name, err)
		}
		if newestName == "" || created.After(newestCreated) {
			newestName = pod.Metadata.Name
			newestCreated = created
		}
	}
	if newestName == "" {
		t.Fatal("no running proxy pod found")
	}
	return newestName
}

func proxyPodImageID(t *testing.T, kubectlCtx, namespace, podName string) string {
	t.Helper()
	out, err := exec.Command(
		"kubectl", "--context", kubectlCtx,
		"-n", namespace, "get", "pod", podName,
		"-o", "jsonpath={.status.containerStatuses[0].imageID}",
	).Output()
	if err != nil {
		t.Fatalf("get imageID for pod %s: %v", podName, err)
	}
	return strings.TrimSpace(string(out))
}

func proxyRolloutRestart(t *testing.T, kubectlCtx, namespace string) {
	t.Helper()
	cmd := exec.Command(
		"kubectl", "--context", kubectlCtx,
		"-n", namespace, "rollout", "restart",
		"deployment/"+proxyDeployment,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rollout restart: %v\n%s", err, string(out))
	}
}

func proxyRolloutWait(t *testing.T, kubectlCtx, namespace string, timeout time.Duration) {
	t.Helper()
	timeoutArg := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	cmd := exec.Command(
		"kubectl", "--context", kubectlCtx,
		"-n", namespace, "rollout", "status",
		"deployment/"+proxyDeployment, timeoutArg,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rollout status: %v\n%s", err, string(out))
	}
}

// proxyStampDigest reads the stamp written by hack/e2e/load-image.sh and
// returns the image digest portion (e.g. "sha256:abc123...").
// Stamp format: IMAGE@sha256:DIGEST.
func proxyStampDigest(t *testing.T, projectDir, kubectlCtx string) string {
	t.Helper()
	clusterName := strings.TrimPrefix(kubectlCtx, "k3d-")
	clusterName = strings.TrimPrefix(clusterName, "kind-")
	stampPath := filepath.Join(projectDir, ".stamps", "e2e", clusterName, "proxy-image.stamp")
	data, err := os.ReadFile(stampPath)
	if err != nil {
		t.Fatalf("read stamp %s: %v", stampPath, err)
	}
	stamp := strings.TrimSpace(string(data))
	parts := strings.SplitN(stamp, "@", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected stamp format %q (expected IMAGE@DIGEST)", stamp)
	}
	return parts[1]
}

func runTask(t *testing.T, projectDir, taskName string, extraArgs ...string) {
	t.Helper()
	args := append([]string{taskName}, extraArgs...)
	cmd := exec.Command("task", args...)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("task %s:\n%s", taskName, string(out))
	}
}
