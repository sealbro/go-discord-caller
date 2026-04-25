package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// MixerMetrics tracks per-tick processing time and end-to-end pipeline latency.
type MixerMetrics struct {
	tickDuration    metric.Float64Histogram
	pipelineLatency metric.Float64Histogram
}

func (m *MixerMetrics) init(meter metric.Meter) (err error) {
	if m.tickDuration, err = meter.Float64Histogram("gdc.mixer.tick.duration",
		metric.WithDescription("Mixer tick processing time"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5, 10, 20),
	); err != nil {
		return
	}
	if m.pipelineLatency, err = meter.Float64Histogram("gdc.mixer.pipeline.latency",
		metric.WithDescription("End-to-end latency from fanout decode to mixer output"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 20, 40, 60, 100, 200, 500),
	); err != nil {
		return
	}
	return nil
}

// RecordTick records a mixer tick duration in milliseconds.
func (m *MixerMetrics) RecordTick(ctx context.Context, ms float64) {
	m.tickDuration.Record(ctx, ms)
}

// RecordPipelineLatency records end-to-end pipeline latency in milliseconds.
func (m *MixerMetrics) RecordPipelineLatency(ctx context.Context, ms float64) {
	m.pipelineLatency.Record(ctx, ms)
}
