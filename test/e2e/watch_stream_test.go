//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWatchStaysOpenThroughProxy(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)

	flunderName := fmt.Sprintf("watch-stream-%d", time.Now().UTC().Unix())
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	watchCtx, cancelWatch := context.WithTimeout(ctx, 90*time.Second)
	defer cancelWatch()

	watchCmd := client.command(watchCtx, "get", "flunders", "-n", "default", "--watch", "--no-headers")
	stdout, err := watchCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("watch stdout pipe: %v", err)
	}
	stderr, err := watchCmd.StderrPipe()
	if err != nil {
		t.Fatalf("watch stderr pipe: %v", err)
	}
	if err := watchCmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}
	t.Cleanup(func() { _ = watchCmd.Wait() })

	seen := make(chan struct{}, 1)
	streamEnded := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), flunderName) {
				seen <- struct{}{}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			streamEnded <- err.Error()
			return
		}
		stderrScanner := bufio.NewScanner(stderr)
		var stderrLines []string
		for stderrScanner.Scan() {
			stderrLines = append(stderrLines, stderrScanner.Text())
		}
		streamEnded <- strings.Join(stderrLines, "\n")
	}()

	time.Sleep(45 * time.Second)
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec:
  reference: long-watch
`, flunderName))

	select {
	case <-seen:
	case reason := <-streamEnded:
		t.Fatalf("watch stream ended before receiving %s: %s", flunderName, reason)
	case <-time.After(30 * time.Second):
		t.Fatalf("watch stayed open but did not stream flunder %s", flunderName)
	}

	cancelWatch()
}
