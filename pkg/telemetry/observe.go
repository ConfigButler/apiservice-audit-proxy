package telemetry

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	outcomeActive = "active"
	labelUnknown  = "unknown"
	protoHTTP2    = "HTTP/2.0"
	protoHTTP2Out = "http2"
	statusDivisor = 100
)

type RequestLabels struct {
	Verb          string
	ResourceGroup string
	Resource      string
	Subresource   string
	Audited       bool
	Streaming     bool
	StatusClass   string
	Outcome       string
	InboundProto  string
	BackendProto  string
}

type BackendLabels struct {
	Verb         string
	Streaming    bool
	Outcome      string
	BackendProto string
}

type StreamLabels struct {
	Kind         string
	Outcome      string
	InboundProto string
	BackendProto string
}

type TransportByteLabels struct {
	Leg       string
	Streaming bool
	Direction string
}

func RecordRequest(ctx context.Context, labels RequestLabels, duration time.Duration) {
	attrs := requestAttrs(labels)
	RequestsTotal.Add(ctx, 1, attrs)
	RequestDurationSeconds.Record(ctx, duration.Seconds(), attrs)
}

func RecordBackendRoundTrip(ctx context.Context, labels BackendLabels, duration time.Duration) {
	BackendRoundTripSeconds.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("verb", labels.Verb),
		attribute.String("streaming", strconv.FormatBool(labels.Streaming)),
		attribute.String("outcome", labels.Outcome),
		attribute.String("backend_proto", normalizeProto(labels.BackendProto)),
	))
}

func AddTransportBytes(ctx context.Context, labels TransportByteLabels, bytes int64) {
	if bytes <= 0 {
		return
	}
	TransportBytesTotal.Add(ctx, bytes, metric.WithAttributes(
		attribute.String("leg", labels.Leg),
		attribute.String("streaming", strconv.FormatBool(labels.Streaming)),
		attribute.String("direction", labels.Direction),
	))
}

func StreamStarted(ctx context.Context, labels StreamLabels) func(string) {
	start := time.Now()
	labels.Outcome = outcomeActive
	activeAttrs := streamAttrs(labels)
	StreamsActive.Add(ctx, 1, activeAttrs)

	return func(outcome string) {
		StreamsActive.Add(ctx, -1, activeAttrs)
		labels.Outcome = outcome
		StreamDurationSeconds.Record(ctx, time.Since(start).Seconds(), streamAttrs(labels))
	}
}

func AddConnection(ctx context.Context, state, proto string, delta int64) {
	ConnectionsActive.Add(ctx, delta, metric.WithAttributes(
		attribute.String("state", state),
		attribute.String("proto", normalizeProto(proto)),
	))
}

func AddAuditEvent(ctx context.Context, outcome string) {
	AuditEventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func RecordAuditDelivery(ctx context.Context, outcome string, duration time.Duration) {
	AuditDeliveryDurationSeconds.Record(ctx, duration.Seconds(),
		metric.WithAttributes(attribute.String("outcome", outcome)))
}

func StatusClass(statusCode int) string {
	if statusCode <= 0 {
		return labelUnknown
	}
	return strconv.Itoa(statusCode/statusDivisor) + "xx"
}

func ProtoLabel(proto string) string {
	return normalizeProto(proto)
}

func requestAttrs(labels RequestLabels) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("verb", emptyToUnknown(labels.Verb)),
		attribute.String("resource_group", emptyToUnknown(labels.ResourceGroup)),
		attribute.String("resource", emptyToUnknown(labels.Resource)),
		attribute.String("subresource", emptyToNone(labels.Subresource)),
		attribute.String("audited", strconv.FormatBool(labels.Audited)),
		attribute.String("streaming", strconv.FormatBool(labels.Streaming)),
		attribute.String("status_class", emptyToUnknown(labels.StatusClass)),
		attribute.String("outcome", emptyToUnknown(labels.Outcome)),
		attribute.String("inbound_proto", normalizeProto(labels.InboundProto)),
		attribute.String("backend_proto", normalizeProto(labels.BackendProto)),
	)
}

func streamAttrs(labels StreamLabels) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("kind", emptyToUnknown(labels.Kind)),
		attribute.String("outcome", emptyToUnknown(labels.Outcome)),
		attribute.String("inbound_proto", normalizeProto(labels.InboundProto)),
		attribute.String("backend_proto", normalizeProto(labels.BackendProto)),
	)
}

func normalizeProto(proto string) string {
	switch proto {
	case protoHTTP2, "h2", protoHTTP2Out:
		return protoHTTP2Out
	case "HTTP/1.1", "HTTP/1.0", "http1":
		return "http1"
	case "":
		return labelUnknown
	default:
		return "other"
	}
}

func emptyToUnknown(value string) string {
	if value == "" {
		return labelUnknown
	}
	return value
}

func emptyToNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
