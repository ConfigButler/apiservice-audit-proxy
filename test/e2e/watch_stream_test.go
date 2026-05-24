//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// regressionSymptoms is the literal stderr fingerprint kubectl prints when
// the proxy tears down a watch via an HTTP/2 reset — exactly the failure
// mode the no-WriteTimeout fix in PR #8 is meant to prevent. If any of
// these appear on stderr, the regression has come back.
var regressionSymptoms = []string{
	"INTERNAL_ERROR",
	"stream error",
	"unexpected EOF",
}

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

	// Drain stderr continuously from process start so we capture the original
	// reset symptom even if it appears mid-watch (not only after stdout ends).
	var (
		stderrMu    sync.Mutex
		stderrLines []string
	)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderrScanner := bufio.NewScanner(stderr)
		// bufio's default 64 KiB cap would drop an over-long line silently and
		// we would lose the very regression symptom this test exists to catch.
		// 1 MiB is far above anything kubectl produces in practice.
		stderrScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for stderrScanner.Scan() {
			stderrMu.Lock()
			stderrLines = append(stderrLines, stderrScanner.Text())
			stderrMu.Unlock()
		}
	}()

	collectedStderr := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		return strings.Join(stderrLines, "\n")
	}

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
		streamEnded <- collectedStderr()
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
	_ = watchCmd.Wait()
	<-stderrDone

	final := collectedStderr()
	for _, symptom := range regressionSymptoms {
		if strings.Contains(final, symptom) {
			t.Fatalf("watch stderr contains regression symptom %q — HTTP/2 reset returned:\n%s",
				symptom, final)
		}
	}
}
