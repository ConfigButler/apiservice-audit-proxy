//go:build e2e

package e2e

// TestAggregatedAPIAuditGap documents and asserts the structural gap in the
// kube-apiserver's native audit trail for requests routed through aggregated
// API servers.
//
// # The problem
//
// When a user writes a resource served by an aggregated API server, the
// kube-apiserver proxies the request opaquely. It emits a native audit event,
// but that event is hollow: the resource name, RequestObject, and
// ResponseObject are all absent. The kube-apiserver sees raw bytes — it has no
// schema to decode what happened on the other side of the proxy boundary.
//
// This is not a kube-apiserver bug. It is a structural property of the
// aggregated API server extension mechanism. The audit trail for those
// resources can only be complete if something at the proxy boundary observes
// and records the interaction — which is exactly what apiservice-audit-proxy
// does.
//
// # What this test asserts
//
// Two webhook-tester sessions run side-by-side (Lane A and Lane B). Both
// receive audit.k8s.io/v1 EventLists for the same write operation:
//
//	Lane A — kube-apiserver native audit:
//	  event IS present (Verb, ObjectRef.Resource, ResponseStatus.Code) — but hollow
//	  ObjectRef.Name:  MISSING (nil/empty)
//	  RequestObject:   MISSING (nil *runtime.Unknown)
//	  ResponseObject:  MISSING (nil *runtime.Unknown)
//
//	Lane B — apiservice-audit-proxy:
//	  event IS present and complete
//	  ObjectRef.Name:  PRESENT (matches the created resource)
//	  RequestObject:   PRESENT (non-nil, Raw contains the submitted manifest)
//	  ResponseObject:  PRESENT (non-nil, Raw contains what the API server stored)
//
// # Reading a failure on the Lane A assertions
//
// The Lane A sub-test asserts the ABSENCE of fields. A failure there does not
// mean the proxy is broken — it means the kube-apiserver has gained the ability
// to audit aggregated API server requests natively. That would be a significant
// upstream change and should trigger a deliberate evaluation of whether the
// proxy is still needed for the audit use case.
//
// # Prerequisites
//
// - The k3d cluster must have been created with audit webhook support baked in
//   (test/e2e/cluster/audit/ files present at cluster creation time).
// - The Helm chart must be deployed with webhookTester.enabled=true.
// - Environment: CTX (kube context), WEBHOOK_TESTER_NAMESPACE (default: wardle).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const (
	// Session UUIDs match test/e2e/cluster/audit/webhook-config.yaml and
	// charts/apiservice-audit-proxy/values.yaml. They are fixed so that browser
	// URLs are stable across cluster recreations.
	auditGapKubeApiserverSessionUUID = "aabbccdd-0000-4000-0000-000000000001"
	auditGapProxySessionUUID         = "aabbccdd-0000-4000-0000-000000000002"

	webhookTesterSvcName   = "apiservice-audit-proxy-webhook-tester"
	webhookTesterLocalPort = "18090"
	webhookTesterSvcPort   = "8080"
)

func TestAggregatedAPIAuditGap(t *testing.T) {
	ctx := context.Background()
	kubectlContext := requireEnv(t, "CTX")

	webhookTesterNamespace := os.Getenv("WEBHOOK_TESTER_NAMESPACE")
	if webhookTesterNamespace == "" {
		webhookTesterNamespace = "wardle"
	}

	client := newKubectlClient(t, kubectlContext)

	// Wait for the aggregated APIService to be reachable before doing anything.
	client.run(ctx,
		"wait", "apiservice/v1alpha1.wardle.example.com",
		`--for=jsonpath={.status.conditions[?(@.type=="Available")].status}=True`,
		"--timeout=240s",
	)

	// Port-forward to webhook-tester. Both session queries go through this tunnel.
	wtURL, stopPF := startPortForwardToServicePort(t, ctx, client, webhookTesterNamespace,
		webhookTesterSvcName, webhookTesterLocalPort, webhookTesterSvcPort)
	defer stopPF()

	// Wait for webhook-tester to respond before clearing sessions.
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

	// Clear both sessions so prior test runs don't pollute the event stream.
	clearWebhookTesterSession(t, wtURL, auditGapKubeApiserverSessionUUID)
	clearWebhookTesterSession(t, wtURL, auditGapProxySessionUUID)

	// Create a Flunder with a unique name so we can identify it in both lanes.
	flunderName := fmt.Sprintf("audit-gap-%d", time.Now().UTC().Unix())
	client.applyYAML(ctx, fmt.Sprintf(`
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: default
spec: {}
`, flunderName))
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", kubectlContext,
			"delete", "flunder", flunderName, "--ignore-not-found", "--wait=false").Run()
	})

	// -------------------------------------------------------------------------
	// Lane B — apiservice-audit-proxy
	//
	// Wait for the proxy to emit a complete event. The proxy observes both sides
	// of the conversation, so it can populate ObjectRef.Name, RequestObject, and
	// ResponseObject. Lane B arriving first also confirms the create succeeded
	// before we evaluate what Lane A captured.
	// -------------------------------------------------------------------------
	t.Log("waiting for Lane B (proxy) to receive the complete audit event...")

	var laneBEvent *auditv1.Event
	waitFor(t, 60*time.Second, func() error {
		events := fetchAuditEvents(t, wtURL, auditGapProxySessionUUID)
		for i := range events {
			if events[i].Verb == "create" &&
				events[i].ObjectRef != nil &&
				events[i].ObjectRef.Name == flunderName {
				ev := events[i]
				laneBEvent = &ev
				return nil
			}
		}
		return fmt.Errorf("no create event for %q in Lane B yet", flunderName)
	})

	t.Run("LaneB_proxy_event_is_complete", func(t *testing.T) {
		if laneBEvent.ObjectRef == nil || laneBEvent.ObjectRef.Name != flunderName {
			t.Errorf("ObjectRef.Name: got %q, want %q",
				func() string {
					if laneBEvent.ObjectRef == nil {
						return "<nil ObjectRef>"
					}
					return laneBEvent.ObjectRef.Name
				}(),
				flunderName,
			)
		}
		if laneBEvent.RequestObject == nil {
			t.Error("RequestObject is nil — proxy must populate the full request body")
		} else {
			t.Logf("Lane B: RequestObject.Raw = %d bytes", len(laneBEvent.RequestObject.Raw))
		}
		if laneBEvent.ResponseObject == nil {
			t.Error("ResponseObject is nil — proxy must populate the full response body")
		} else {
			t.Logf("Lane B: ResponseObject.Raw = %d bytes", len(laneBEvent.ResponseObject.Raw))
		}
	})

	// -------------------------------------------------------------------------
	// Lane A — kube-apiserver native audit
	//
	// The batch-max-wait is 1 s. Give it a few seconds beyond that to arrive
	// after Lane B has confirmed the request completed.
	// -------------------------------------------------------------------------
	time.Sleep(3 * time.Second)

	t.Run("LaneA_kube_apiserver_event_is_hollow", func(t *testing.T) {
		laneAEvents := fetchAuditEvents(t, wtURL, auditGapKubeApiserverSessionUUID)

		// Find a wardle create-flunders event in Lane A. We cannot match by name
		// because ObjectRef.Name is one of the fields that should be absent.
		var laneAEvent *auditv1.Event
		for i := range laneAEvents {
			ev := &laneAEvents[i]
			if ev.Verb == "create" &&
				ev.ObjectRef != nil &&
				ev.ObjectRef.Resource == "flunders" &&
				ev.ObjectRef.APIGroup == "wardle.example.com" {
				laneAEvent = ev
				break
			}
		}

		if laneAEvent == nil {
			// The kube-apiserver emitted no audit event at all. This is a more
			// severe form of the gap — complete invisibility rather than a hollow
			// record. Either outcome demonstrates why the proxy is needed.
			t.Log("Lane A: kube-apiserver emitted NO audit event for the aggregated API " +
				"request — the operation is completely invisible to the native audit trail")
			return
		}

		t.Logf("Lane A: event found — Verb=%s ObjectRef.Resource=%s ResponseStatus.Code=%d"+
			" (asserting missing fields...)",
			laneAEvent.Verb,
			laneAEvent.ObjectRef.Resource,
			func() int32 {
				if laneAEvent.ResponseStatus != nil {
					return laneAEvent.ResponseStatus.Code
				}
				return 0
			}(),
		)

		// Assert ObjectRef.Name is absent.
		//
		// The kube-apiserver proxies the request without decoding it, so it
		// cannot extract the resource name from the body or the response.
		if laneAEvent.ObjectRef.Name != "" {
			t.Errorf(
				"UNEXPECTED: Lane A event has ObjectRef.Name=%q\n"+
					"The kube-apiserver can now resolve resource names for aggregated API "+
					"requests. Re-evaluate whether the proxy is still needed for name population.",
				laneAEvent.ObjectRef.Name,
			)
		} else {
			t.Log("Lane A: ObjectRef.Name is absent (expected — kube-apiserver cannot resolve it through the proxy)")
		}

		// Assert RequestObject is absent.
		//
		// At RequestResponse audit level the kube-apiserver would need to
		// unmarshal the body into a typed object — which it cannot do for a type
		// defined only in the aggregated API server. The field stays nil.
		if laneAEvent.RequestObject != nil {
			t.Errorf(
				"UNEXPECTED: Lane A event has a populated RequestObject (%d bytes raw)\n"+
					"The kube-apiserver can now decode aggregated API request bodies. "+
					"Re-evaluate whether the proxy is still needed for RequestObject population.",
				len(laneAEvent.RequestObject.Raw),
			)
		} else {
			t.Log("Lane A: RequestObject is nil (expected — kube-apiserver cannot decode aggregated types)")
		}

		// Assert ResponseObject is absent.
		if laneAEvent.ResponseObject != nil {
			t.Errorf(
				"UNEXPECTED: Lane A event has a populated ResponseObject (%d bytes raw)\n"+
					"The kube-apiserver can now decode aggregated API response bodies. "+
					"Re-evaluate whether the proxy is still needed for ResponseObject population.",
				len(laneAEvent.ResponseObject.Raw),
			)
		} else {
			t.Log("Lane A: ResponseObject is nil (expected — kube-apiserver cannot decode aggregated types)")
		}
	})
}

// --- webhook-tester helpers --------------------------------------------------

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
		return nil // session not yet created — no events yet
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
// service port (the smoke test helper is fixed to port 9444).
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
