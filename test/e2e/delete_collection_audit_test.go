//go:build e2e

package e2e

// TestAggregatedAPIDeleteCollectionAudit asserts that the proxy emits a
// complete audit event for a collection delete (the distinct "deletecollection"
// verb) of an aggregated resource — the case that regressed silently before the
// shouldAudit allow-list included it.
//
// # Why this matters
//
// A DELETE against a resource path with no object name is rewritten by the
// kube-apiserver's RequestInfo to the verb "deletecollection" (it is NOT folded
// into "delete"). A frequent real-cluster trigger is namespace teardown:
// deleting a namespace issues a deletecollection for every namespaced resource.
// For aggregated resources the kube-apiserver's own audit event is hollow (see
// audit_gap_test.go), so the proxy is the only component that can record the
// operation for a downstream consumer.
//
// # Trigger
//
// `kubectl delete --raw` against the collection path issues a single HTTP DELETE
// with no name, which is exactly what becomes verb=deletecollection. This is
// deterministic, unlike `kubectl delete <type> --all`, which lists the
// collection and then deletes each member individually (verb=delete per name).
//
// # What this test asserts (Lane B — proxy)
//
//	event IS present for verb=deletecollection on the flunders resource
//	ObjectRef.Name:       ABSENT (a collection op has no single object)
//	ObjectRef.Resource:   PRESENT ("flunders")
//	ObjectRef.APIGroup:   PRESENT ("wardle.example.com")
//	ObjectRef.Namespace:  PRESENT (the test namespace)
//	ResponseStatus.Code:  200 (the operation succeeded)
//	ResponseObject:       PRESENT — for a raw collection delete the wardle
//	                      backend returns the FlunderList of deleted items.
//
// Note on the body: the body content is caller-dependent and is NOT what
// downstream consumers key off (they use ObjectRef). A raw collection delete
// returns the deleted-items list, whereas the namespace-controller's
// deletecollection returns an empty 200 body — in that case ResponseObject is
// legitimately nil and only ResponseStatus carries the outcome. The test logs
// the decoded body so the real wire shape is visible in the output.
//
// # Prerequisites — same as TestAggregatedAPIAuditGap
//
// - Cluster created with audit webhook support baked in.
// - Helm chart deployed with webhookTester.enabled=true.
// - Environment: CTX (kube context), WEBHOOK_TESTER_BASE_URL.

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func TestAggregatedAPIDeleteCollectionAudit(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")
	client := newKubectlClient(t, kubectlContext)

	waitForWardleAPIService(t, client)

	wtURL := webhookTesterBaseURL()
	waitForWebhookTester(t, wtURL)
	clearWebhookTesterSession(t, wtURL, auditGapProxySessionUUID)

	// Use a dedicated namespace so the collection delete is isolated from
	// flunders other tests may create in the default namespace.
	ns := fmt.Sprintf("delcoll-%d", time.Now().UTC().Unix())
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, ns))
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "namespace", ns, "--ignore-not-found", "--wait=false").Run()
	})

	// Seed two flunders so the collection delete has real members to remove.
	for i := 0; i < 2; i++ {
		client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: member-%d
  namespace: %s
spec:
  reference: delete-collection
`, i, ns))
	}

	// Trigger the collection delete: a single raw DELETE against the collection
	// path with no name → deletecollection.
	collectionPath := fmt.Sprintf("/apis/wardle.example.com/v1alpha1/namespaces/%s/flunders", ns)
	client.run(ctx, "delete", "--raw", collectionPath)

	t.Log("waiting for Lane B (proxy) to receive the deletecollection audit event...")

	var event *auditv1.Event
	waitFor(t, 60*time.Second, func() error {
		events := fetchAuditEvents(t, wtURL, auditGapProxySessionUUID)
		for i := range events {
			ev := &events[i]
			if ev.Verb == "deletecollection" &&
				ev.ObjectRef != nil &&
				ev.ObjectRef.Resource == "flunders" &&
				ev.ObjectRef.APIGroup == "wardle.example.com" &&
				ev.ObjectRef.Namespace == ns {
				event = ev
				return nil
			}
		}
		return fmt.Errorf("no deletecollection event for flunders in namespace %q yet", ns)
	})

	t.Run("proxy_event_is_complete", func(t *testing.T) {
		// A collection op targets no single object — there must be no name.
		if event.ObjectRef.Name != "" {
			t.Errorf("ObjectRef.Name: got %q, want empty (a collection delete has no single object)",
				event.ObjectRef.Name)
		}
		if event.ObjectRef.Resource != "flunders" {
			t.Errorf("ObjectRef.Resource: got %q, want %q", event.ObjectRef.Resource, "flunders")
		}
		if event.ObjectRef.APIGroup != "wardle.example.com" {
			t.Errorf("ObjectRef.APIGroup: got %q, want %q", event.ObjectRef.APIGroup, "wardle.example.com")
		}
		if event.ObjectRef.Namespace != ns {
			t.Errorf("ObjectRef.Namespace: got %q, want %q", event.ObjectRef.Namespace, ns)
		}

		// The operation must be recorded as successful.
		if event.ResponseStatus == nil || event.ResponseStatus.Code != 200 {
			gotCode := int32(0)
			if event.ResponseStatus != nil {
				gotCode = event.ResponseStatus.Code
			}
			t.Errorf("ResponseStatus.Code: got %d, want 200", gotCode)
		}

		// A raw collection delete returns the list of deleted items, so the
		// proxy must capture a usable response body rather than dropping it.
		if event.ResponseObject == nil {
			t.Error("ResponseObject is nil — proxy must capture the deletecollection response body")
		} else {
			t.Logf("Lane B: deletecollection ResponseObject.Raw (%d bytes) = %s",
				len(event.ResponseObject.Raw), string(event.ResponseObject.Raw))
		}
		t.Logf("Lane B: deletecollection event recorded — user=%q", event.User.Username)
	})
}
