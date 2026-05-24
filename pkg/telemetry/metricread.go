package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func CollectInt64Sum(
	reader *sdkmetric.ManualReader,
	metricName string,
	match map[string]string,
) (int64, bool) {
	data, found := collectMetric(reader, metricName)
	if !found {
		return 0, false
	}

	var value int64
	var ok bool
	switch agg := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range agg.DataPoints {
			if attrsMatch(dp.Attributes, match) {
				value += dp.Value
				ok = true
			}
		}
	case metricdata.Gauge[int64]:
		for _, dp := range agg.DataPoints {
			if attrsMatch(dp.Attributes, match) {
				value = dp.Value
				ok = true
			}
		}
	}

	return value, ok
}

func CollectHistogramCount(
	reader *sdkmetric.ManualReader,
	metricName string,
	match map[string]string,
) (uint64, bool) {
	data, found := collectMetric(reader, metricName)
	if !found {
		return 0, false
	}
	agg, isHistogram := data.(metricdata.Histogram[float64])
	if !isHistogram {
		return 0, false
	}

	var count uint64
	var ok bool
	for _, dp := range agg.DataPoints {
		if attrsMatch(dp.Attributes, match) {
			count += dp.Count
			ok = true
		}
	}

	return count, ok
}

func collectMetric(reader *sdkmetric.ManualReader, metricName string) (metricdata.Aggregation, bool) {
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		return nil, false
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == metricName {
				return m.Data, true
			}
		}
	}
	return nil, false
}

func attrsMatch(set attribute.Set, match map[string]string) bool {
	for key, want := range match {
		got, present := set.Value(attribute.Key(key))
		if !present || got.AsString() != want {
			return false
		}
	}
	return true
}
