//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildImageTask_RebuildsOnGoSourceChange verifies that task e2e:build-image
// produces a new image after a Go source file is modified.
//
// The test uses the default local image tag so the status guard (which skips
// builds for externally-provided images) does not short-circuit the rebuild.
func TestBuildImageTask_RebuildsOnGoSourceChange(t *testing.T) {
	// Use the default local tag — this is the code path where rebuilds must happen.
	const defaultImage = "apiservice-audit-proxy:e2e-local"

	projectDir := findProjectRoot(t)

	// Ensure a baseline image exists before we record id1.
	runTaskBuildImage(t, projectDir, defaultImage)
	id1 := dockerImageID(t, defaultImage)

	// Append a harmless comment to a build-path Go file to bust the Docker layer cache.
	srcFile := filepath.Join(projectDir, "cmd", "server", "main.go")
	original, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source file: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(srcFile, original, 0600) })
	if err := os.WriteFile(srcFile, append(original, []byte("\n// taskfile-rebuild-test\n")...), 0600); err != nil {
		t.Fatalf("modify source file: %v", err)
	}

	runTaskBuildImage(t, projectDir, defaultImage)
	id2 := dockerImageID(t, defaultImage)

	if id1 == id2 {
		t.Fatal("image ID unchanged after Go source file was modified: rebuild did not occur")
	}
}

// TestBuildImageTask_SkipsForExternalImage verifies that task e2e:build-image
// is a no-op when E2E_PROXY_IMAGE is set to any value other than the default
// local tag. This is the CI path where the image is pre-built and pushed.
func TestBuildImageTask_SkipsForExternalImage(t *testing.T) {
	const externalImage = "apiservice-audit-proxy:taskfile-external-test"

	projectDir := findProjectRoot(t)
	removeImage(t, externalImage)

	runTaskBuildImage(t, projectDir, externalImage)

	if err := exec.Command("docker", "image", "inspect", externalImage).Run(); err == nil {
		t.Fatal("task e2e:build-image built the image despite non-default E2E_PROXY_IMAGE: status guard broken")
	}
}

func runTaskBuildImage(t *testing.T, projectDir, image string) {
	t.Helper()
	cmd := exec.Command("task", "e2e:build-image", "E2E_PROXY_IMAGE="+image)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("task e2e:build-image E2E_PROXY_IMAGE=%s:\n%s", image, string(out))
	}
}

func dockerImageID(t *testing.T, image string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format={{.Id}}", image).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", image, err)
	}
	return strings.TrimSpace(string(out))
}

func removeImage(t *testing.T, image string) {
	t.Helper()
	_ = exec.Command("docker", "rmi", "--force", image).Run()
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "Taskfile.yml")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("project root not found: Taskfile.yml not in any parent directory")
		}
		dir = parent
	}
}
