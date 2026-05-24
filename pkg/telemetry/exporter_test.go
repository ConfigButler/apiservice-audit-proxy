package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitTestExporterAndCollect(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	ctx := context.Background()
	RecordRequest(ctx, RequestLabels{
		Verb:          "watch",
		ResourceGroup: "wardle.example.com",
		APIVersion:    "v1alpha1",
		Resource:      "flunders",
		Audited:       false,
		Streaming:     true,
		StatusClass:   "2xx",
		Outcome:       "ok",
		InboundProto:  "HTTP/2.0",
		BackendProto:  "HTTP/2.0",
	}, 25*time.Millisecond)
	AddTransportBytes(ctx, TransportByteLabels{Leg: "client", Streaming: true, Direction: "write"}, 128)
	RecordBackendRoundTrip(ctx, BackendLabels{
		Verb:         "watch",
		Streaming:    true,
		Outcome:      "ok",
		BackendProto: "HTTP/2.0",
	}, 10*time.Millisecond)

	requests, ok := CollectInt64Sum(reader, "apiservice_audit_proxy_requests_total", map[string]string{
		"verb":           "watch",
		"resource_group": "wardle.example.com",
		"api_version":    "v1alpha1",
		"resource":       "flunders",
		"streaming":      "true",
		"inbound_proto":  "http2",
		"backend_proto":  "http2",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), requests)

	bytes, ok := CollectInt64Sum(reader, "apiservice_audit_proxy_transport_bytes_total", map[string]string{
		"leg":       "client",
		"streaming": "true",
		"direction": "write",
	})
	require.True(t, ok)
	assert.Equal(t, int64(128), bytes)

	requestDurationCount, ok := CollectHistogramCount(reader,
		"apiservice_audit_proxy_request_duration_seconds",
		map[string]string{"verb": "watch"})
	require.True(t, ok)
	assert.Equal(t, uint64(1), requestDurationCount)

	backendDurationCount, ok := CollectHistogramCount(reader,
		"apiservice_audit_proxy_backend_roundtrip_seconds",
		map[string]string{"verb": "watch"})
	require.True(t, ok)
	assert.Equal(t, uint64(1), backendDurationCount)
}

func TestStreamStartedRecordsActiveGaugeAndDuration(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	done := StreamStarted(context.Background(), StreamLabels{
		Kind:         "watch",
		InboundProto: "HTTP/2.0",
		BackendProto: "HTTP/2.0",
	})

	active, ok := CollectInt64Sum(reader, "apiservice_audit_proxy_streams_active", map[string]string{
		"kind":          "watch",
		"inbound_proto": "http2",
		"backend_proto": "http2",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), active)

	done("client_cancel")

	active, ok = CollectInt64Sum(reader, "apiservice_audit_proxy_streams_active", map[string]string{
		"kind":          "watch",
		"inbound_proto": "http2",
		"backend_proto": "http2",
	})
	require.True(t, ok)
	assert.Equal(t, int64(0), active)

	durationCount, ok := CollectHistogramCount(reader,
		"apiservice_audit_proxy_stream_duration_seconds",
		map[string]string{"kind": "watch", "outcome": "client_cancel"})
	require.True(t, ok)
	assert.Equal(t, uint64(1), durationCount)
}
