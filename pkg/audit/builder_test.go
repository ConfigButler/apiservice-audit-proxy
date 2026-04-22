package audit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
)

func TestBuilder_BuildResponseCompleteEvent(t *testing.T) {
	t.Parallel()

	requestBody := []byte(
		`{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default"},"spec":{"reference":"alpha"}}`,
	)
	responseBody := []byte(
		`{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default","uid":"uid-123","resourceVersion":"7"},"spec":{"reference":"alpha"}}`,
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders?fieldManager=kubectl",
		nil,
	)
	req.Header.Set("Audit-Id", "audit-123")
	req.Header.Set("User-Agent", "kubectl/v1.33.0")
	req.RemoteAddr = "10.0.0.5:12345"

	resolver := &requestinfo.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
	info, err := resolver.NewRequestInfo(req)
	require.NoError(t, err)

	builder := NewBuilder(1024)
	event, err := builder.Build(Input{
		Request:             req,
		RequestInfo:         info,
		User:                testUser(),
		RequestBody:         requestBody,
		RequestBodyBytes:    int64(len(requestBody)),
		ResponseBody:        responseBody,
		ResponseBodyBytes:   int64(len(responseBody)),
		ResponseStatusCode:  201,
		RequestReceivedAt:   time.Date(2026, 4, 20, 11, 0, 0, 0, time.UTC),
		ResponseCompletedAt: time.Date(2026, 4, 20, 11, 0, 1, 0, time.UTC),
	})
	require.NoError(t, err)

	gotJSON := mustMarshalJSON(t, Wrap(*event))
	wantJSON, err := os.ReadFile(filepath.Join("testdata", "response_complete_event.json"))
	require.NoError(t, err)

	require.JSONEq(t, string(wantJSON), string(gotJSON))
}

func TestBuilder_TruncatedBodies_UsePlaceholderAndAnnotations(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(
		http.MethodPatch,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders/audit-probe",
		nil,
	)

	resolver := &requestinfo.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
	info, err := resolver.NewRequestInfo(req)
	require.NoError(t, err)

	builder := NewBuilder(16)
	event, err := builder.Build(Input{
		Request:               req,
		RequestInfo:           info,
		User:                  testUser(),
		RequestBody:           []byte(`{"metadata":{"`),
		RequestBodyBytes:      256,
		RequestBodyTruncated:  true,
		ResponseBody:          []byte(`{"metadata":{"`),
		ResponseBodyBytes:     512,
		ResponseBodyTruncated: true,
		ResponseStatusCode:    200,
		RequestReceivedAt:     time.Date(2026, 4, 20, 11, 0, 0, 0, time.UTC),
		ResponseCompletedAt:   time.Date(2026, 4, 20, 11, 0, 1, 0, time.UTC),
	})
	require.NoError(t, err)
	require.NotNil(t, event.RequestObject)
	require.NotNil(t, event.ResponseObject)
	require.NotNil(t, event.Annotations)
	require.Equal(t, "true", event.Annotations[truncationAnnotationPrefix+"request-truncated"])
	require.Equal(t, "true", event.Annotations[truncationAnnotationPrefix+"response-truncated"])

	requestObject := map[string]any{}
	require.NoError(t, json.Unmarshal(event.RequestObject.Raw, &requestObject))
	require.Equal(t, true, requestObject["truncated"])
	require.EqualValues(t, 256, requestObject["originalSizeBytes"])
}

func testUser() authnv1.UserInfo {
	return authnv1.UserInfo{
		Username: "alice@example.com",
		UID:      "uid-alice",
		Groups:   []string{"devs", "admins"},
		Extra: map[string]authnv1.ExtraValue{
			"example.com/tenant": {"team-a"},
		},
	}
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)

	return data
}
