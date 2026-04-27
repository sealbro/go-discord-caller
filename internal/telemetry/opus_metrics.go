package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OpusMetrics tracks per-frame timing for the voice pipeline and mixer.
type OpusMetrics struct {
	receiveDuration   metric.Float64Histogram
	provideDuration   metric.Float64Histogram
	allowUserDuration metric.Float64Histogram
	tickDuration      metric.Float64Histogram
	pipelineLatency   metric.Float64Histogram
}

func (o *OpusMetrics) init(meter metric.Meter) (err error) {
	if o.receiveDuration, err = meter.Float64Histogram("gdc.opus.receive.duration",
		metric.WithDescription("ReceiveOpusFrame execution duration (excluding time blocked waiting for channel)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 1, 2, 5, 10),
	); err != nil {
		return
	}
	if o.provideDuration, err = meter.Float64Histogram("gdc.opus.provide.duration",
		metric.WithDescription("ProvideOpusFrame execution duration (excluding time blocked waiting for a frame)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 1, 2, 5, 10),
	); err != nil {
		return
	}
	if o.allowUserDuration, err = meter.Float64Histogram("gdc.opus.allow_user.duration",
		metric.WithDescription("allowUser filter execution duration per evaluated frame"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 5, 10),
	); err != nil {
		return
	}
	if o.tickDuration, err = meter.Float64Histogram("gdc.mixer.tick.duration",
		metric.WithDescription("Mixer tick processing time"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5, 10, 20),
	); err != nil {
		return
	}
	if o.pipelineLatency, err = meter.Float64Histogram("gdc.mixer.pipeline.latency",
		metric.WithDescription("End-to-end latency from fanout decode to mixer output"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 20, 40, 60, 100, 200, 500),
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
	drop func() // optional drop counter; set via WithDrop
}

// For returns an OpusRecorder with guild_id pre-baked into every measurement.
func (o *OpusMetrics) For(guildID string) OpusRecorder {
	return OpusRecorder{
		m:    o,
		attr: metric.WithAttributes(attribute.String("guild_id", guildID)),
	}
}

// Active reports whether this recorder has an underlying meter (non-zero value).
func (r OpusRecorder) Active() bool { return r.m != nil }

// WithDrop returns a copy of the recorder with fn registered as the drop callback.
// fn is called once per frame dropped on the hot path; pass nil to clear.
func (r OpusRecorder) WithDrop(fn func()) OpusRecorder {
	r.drop = fn
	return r
}

// RecordDrop invokes the registered drop callback (if any).
func (r OpusRecorder) RecordDrop() {
	if r.drop != nil {
		r.drop()
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

// RecordTick records a mixer tick duration in milliseconds.
func (r OpusRecorder) RecordTick(ms float64) {
	if r.m == nil {
		return
	}
	r.m.tickDuration.Record(context.Background(), ms, r.attr)
}

// RecordPipelineLatency records end-to-end pipeline latency in milliseconds.
func (r OpusRecorder) RecordPipelineLatency(ms float64) {
	if r.m == nil {
		return
	}
	r.m.pipelineLatency.Record(context.Background(), ms, r.attr)
}
