package telemetry

import (
	"context"
	"fmt"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const meterName = "apiservice-audit-proxy"

//nolint:gochecknoglobals // OpenTelemetry instruments are process-wide handles, mirroring GitOps Reverser.
var (
	meter metric.Meter

	RequestsTotal                metric.Int64Counter
	RequestDurationSeconds       metric.Float64Histogram
	BackendRoundTripSeconds      metric.Float64Histogram
	StreamsActive                metric.Int64UpDownCounter
	StreamDurationSeconds        metric.Float64Histogram
	TransportBytesTotal          metric.Int64Counter
	ConnectionsActive            metric.Int64UpDownCounter
	AuditEventsTotal             metric.Int64Counter
	AuditDeliveryDurationSeconds metric.Float64Histogram
)

type counterSpec struct {
	name string
	dest *metric.Int64Counter
}

type histogramSpec struct {
	name    string
	dest    *metric.Float64Histogram
	buckets []float64
}

type upDownSpec struct {
	name string
	dest *metric.Int64UpDownCounter
}

//nolint:gochecknoinits // Keep package-level instruments safe before explicit exporter initialization.
func init() {
	meter = otel.Meter(meterName)
	_ = registerInstruments()
}

func InitPrometheusExporter(_ context.Context, registerer promclient.Registerer) (func(context.Context) error, error) {
	opts := []otelprom.Option{}
	if registerer != nil {
		opts = append(opts, otelprom.WithRegisterer(registerer))
	}

	exporter, err := otelprom.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create prometheus metrics exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)
	meter = provider.Meter(meterName)
	if err := registerInstruments(); err != nil {
		return nil, err
	}

	return provider.Shutdown, nil
}

func InitTestExporter() (*sdkmetric.ManualReader, error) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	meter = provider.Meter(meterName)
	if err := registerInstruments(); err != nil {
		return nil, err
	}

	return reader, nil
}

func registerInstruments() error {
	counters := []counterSpec{
		{"apiservice_audit_proxy_requests_total", &RequestsTotal},
		{"apiservice_audit_proxy_transport_bytes_total", &TransportBytesTotal},
		{"apiservice_audit_proxy_audit_events_total", &AuditEventsTotal},
	}
	for _, spec := range counters {
		instrument, err := meter.Int64Counter(spec.name)
		if err != nil {
			return err
		}
		*spec.dest = instrument
	}

	requestBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	streamBuckets := []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	histograms := []histogramSpec{
		{"apiservice_audit_proxy_request_duration_seconds", &RequestDurationSeconds, requestBuckets},
		{"apiservice_audit_proxy_backend_roundtrip_seconds", &BackendRoundTripSeconds, requestBuckets},
		{"apiservice_audit_proxy_stream_duration_seconds", &StreamDurationSeconds, streamBuckets},
		{"apiservice_audit_proxy_audit_delivery_duration_seconds", &AuditDeliveryDurationSeconds, requestBuckets},
	}
	for _, spec := range histograms {
		instrument, err := meter.Float64Histogram(
			spec.name,
			metric.WithExplicitBucketBoundaries(spec.buckets...),
		)
		if err != nil {
			return err
		}
		*spec.dest = instrument
	}

	upDowns := []upDownSpec{
		{"apiservice_audit_proxy_streams_active", &StreamsActive},
		{"apiservice_audit_proxy_connections_active", &ConnectionsActive},
	}
	for _, spec := range upDowns {
		instrument, err := meter.Int64UpDownCounter(spec.name)
		if err != nil {
			return err
		}
		*spec.dest = instrument
	}

	return nil
}
