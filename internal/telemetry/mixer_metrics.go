package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
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

// MixerRecorder is a pre-baked recorder for a specific guild_id.
// Obtain one via MixerMetrics.For and reuse it for the lifetime of a session.
// All record calls are zero-alloc on the hot path.
type MixerRecorder struct {
	m    *MixerMetrics
	attr metric.MeasurementOption
}

// For returns a MixerRecorder with guild_id pre-baked into every measurement.
func (m *MixerMetrics) For(guildID string) MixerRecorder {
	return MixerRecorder{
		m:    m,
		attr: metric.WithAttributes(attribute.String("guild_id", guildID)),
	}
}

// RecordTick records a mixer tick duration in milliseconds.
func (r MixerRecorder) RecordTick(ctx context.Context, ms float64) {
	r.m.tickDuration.Record(ctx, ms, r.attr)
}

// RecordPipelineLatency records end-to-end pipeline latency in milliseconds.
func (r MixerRecorder) RecordPipelineLatency(ctx context.Context, ms float64) {
	r.m.pipelineLatency.Record(ctx, ms, r.attr)
}
