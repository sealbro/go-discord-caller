package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OpusMetrics tracks per-frame timing for the voice pipeline.
type OpusMetrics struct {
	receiveDuration   metric.Float64Histogram
	provideDuration   metric.Float64Histogram
	allowUserDuration metric.Float64Histogram
}

func (o *OpusMetrics) init(meter metric.Meter) (err error) {
	if o.receiveDuration, err = meter.Float64Histogram("gdc.opus.receive.duration",
		metric.WithDescription("ReceiveOpusFrame execution duration (excluding time blocked waiting for channel)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5),
	); err != nil {
		return
	}
	if o.provideDuration, err = meter.Float64Histogram("gdc.opus.provide.duration",
		metric.WithDescription("ProvideOpusFrame execution duration (excluding time blocked waiting for a frame)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5),
	); err != nil {
		return
	}
	if o.allowUserDuration, err = meter.Float64Histogram("gdc.opus.allow_user.duration",
		metric.WithDescription("allowUser filter execution duration per evaluated frame"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1),
	); err != nil {
		return
	}
	return nil
}

// OpusRecorder is a pre-baked recorder for a specific guild_id.
// Obtain one via OpusMetrics.For and reuse it for the lifetime of a session.
// All record calls are zero-alloc on the hot path.
type OpusRecorder struct {
	m    *OpusMetrics
	attr metric.MeasurementOption
}

// For returns an OpusRecorder with guild_id pre-baked into every measurement.
func (o *OpusMetrics) For(guildID string) OpusRecorder {
	return OpusRecorder{
		m:    o,
		attr: metric.WithAttributes(attribute.String("guild_id", guildID)),
	}
}

// RecordReceive records the execution duration of one ReceiveOpusFrame call.
func (r OpusRecorder) RecordReceive(ms float64) {
	if r.m == nil {
		return
	}
	r.m.receiveDuration.Record(context.Background(), ms, r.attr)
}

// RecordProvide records the execution duration of one ProvideOpusFrame drain+return path.
func (r OpusRecorder) RecordProvide(ms float64) {
	if r.m == nil {
		return
	}
	r.m.provideDuration.Record(context.Background(), ms, r.attr)
}

// RecordAllowUser records the execution duration of one allowUser filter call.
func (r OpusRecorder) RecordAllowUser(ms float64) {
	if r.m == nil {
		return
	}
	r.m.allowUserDuration.Record(context.Background(), ms, r.attr)
}
