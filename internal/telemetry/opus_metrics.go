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
	tickLateness      metric.Float64Histogram
	pipelineLatency   metric.Float64Histogram
	copyQueueDepth    metric.Int64Histogram
}

func (o *OpusMetrics) init(meter metric.Meter) (err error) {
	if o.receiveDuration, err = meter.Float64Histogram("gdc.opus.receive.duration",
		metric.WithDescription("ReceiveOpusFrame execution duration (excluding time blocked waiting for channel)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 12, 20),
	); err != nil {
		return
	}
	if o.provideDuration, err = meter.Float64Histogram("gdc.opus.provide.duration",
		metric.WithDescription("ProvideOpusFrame execution duration (excluding time blocked waiting for a frame)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 12, 20),
	); err != nil {
		return
	}
	if o.allowUserDuration, err = meter.Float64Histogram("gdc.opus.allow_user.duration",
		metric.WithDescription("allowUser lookup duration on a per-session cache miss (once per user, not per frame)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 12, 20),
	); err != nil {
		return
	}
	if o.tickDuration, err = meter.Float64Histogram("gdc.mixer.tick.duration",
		metric.WithDescription("Mixer tick processing time"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20),
	); err != nil {
		return
	}
	// A tick fires on time to within a millisecond or two; the upper buckets
	// exist for scheduler or GC stalls, which are what push a queued frame past
	// the 60 ms cap in gdc.mixer.pipeline.latency.
	if o.tickLateness, err = meter.Float64Histogram("gdc.mixer.tick.lateness",
		metric.WithDescription("How far past its 20 ms boundary a mixer tick fired"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 40, 80),
	); err != nil {
		return
	}
	// Copy mode bypasses the mixer, so the pipeline-latency histogram never sees
	// it; a copied frame's wait is the frames queued ahead of it in the
	// speaker's output channel, which disgo drains one per 20 ms.
	if o.copyQueueDepth, err = meter.Int64Histogram("gdc.opus.copy.queue_depth",
		metric.WithDescription("Frames already queued ahead of a copy-mode frame in a speaker's output channel; each is ~20 ms of wait"),
		metric.WithUnit("{frame}"),
		metric.WithExplicitBucketBoundaries(0, 1, 2, 3),
	); err != nil {
		return
	}
	// Boundaries are clustered between 15 and 60 ms because that is where the
	// distribution actually lives: the floor is ~20 ms (a frame waits for the
	// next mixer tick) and the ceiling is 60 ms (audioSourceCap = 3 frames).
	// The previous 10/30/50/70/100/200/500 layout put two thirds of all
	// observations inside the single 10-30 ms bucket, so every quantile below
	// p95 was linear interpolation across the whole interesting range, while
	// the 100/200/500 buckets held under 0.01 % between them.
	if o.pipelineLatency, err = meter.Float64Histogram("gdc.mixer.pipeline.latency",
		metric.WithDescription("Latency from Discord receive to the mixer tick that consumes the frame"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(10, 15, 20, 25, 30, 35, 40, 50, 60, 80, 120),
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

// RecordTickLateness records how late a mixer tick fired, in milliseconds.
func (r OpusRecorder) RecordTickLateness(ms float64) {
	if r.m == nil {
		return
	}
	r.m.tickLateness.Record(context.Background(), ms, r.attr)
}

// RecordCopyQueue records how many frames were queued ahead of a copy-mode
// frame when it entered a speaker's output channel.
func (r OpusRecorder) RecordCopyQueue(ahead int) {
	if r.m == nil {
		return
	}
	r.m.copyQueueDepth.Record(context.Background(), int64(ahead), r.attr)
}

// RecordPipelineLatency records end-to-end pipeline latency in milliseconds.
func (r OpusRecorder) RecordPipelineLatency(ms float64) {
	if r.m == nil {
		return
	}
	r.m.pipelineLatency.Record(context.Background(), ms, r.attr)
}
