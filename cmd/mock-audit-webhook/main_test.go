package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func TestHandleWebhook_StoresPayloadAndServesItBack(t *testing.T) {
	t.Parallel()

	store := newEventStore(5)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", serveEvents(store))
	mux.HandleFunc("/", handleWebhook(store, slog.New(slog.NewTextHandler(io.Discard, nil))))

	server := httptest.NewServer(mux)
	defer server.Close()

	payload := auditv1.EventList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "audit.k8s.io/v1",
			Kind:       "EventList",
		},
		Items: []auditv1.Event{
			{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "audit.k8s.io/v1",
					Kind:       "Event",
				},
				Verb: "create",
			},
		},
	}

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/audit-webhook/test", bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	eventsResp, err := http.Get(server.URL + "/events")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, eventsResp.Body.Close())
	}()
	require.Equal(t, http.StatusOK, eventsResp.StatusCode)

	var listed struct {
		Items []storedEvent `json:"items"`
	}
	require.NoError(t, json.NewDecoder(eventsResp.Body).Decode(&listed))
	require.Len(t, listed.Items, 1)
	require.Equal(t, "/audit-webhook/test", listed.Items[0].Path)
	require.Len(t, listed.Items[0].EventList.Items, 1)
	require.Equal(t, "create", listed.Items[0].EventList.Items[0].Verb)
}
