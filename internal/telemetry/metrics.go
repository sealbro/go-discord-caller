package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meter = otel.Meter(serviceName)

// Command metrics
var (
	CommandDuration metric.Float64Histogram
	CommandCount    metric.Int64Counter
)

// Voice session metrics
var (
	SessionsActive metric.Int64UpDownCounter
	SessionStart   metric.Int64Counter
	SessionStop    metric.Int64Counter
)

// Mixer metrics
var (
	MixerTickDuration    metric.Float64Histogram
	MixerPipelineLatency metric.Float64Histogram
)

func init() {
	var err error

	CommandDuration, err = meter.Float64Histogram("gdc.command.duration",
		metric.WithDescription("Slash command execution duration in seconds"),
		metric.WithUnit("s"),
	)
	must(err)

	CommandCount, err = meter.Int64Counter("gdc.command.total",
		metric.WithDescription("Slash command invocations"),
	)
	must(err)

	SessionsActive, err = meter.Int64UpDownCounter("gdc.voice.sessions.active",
		metric.WithDescription("Currently active voice raid sessions"),
	)
	must(err)

	SessionStart, err = meter.Int64Counter("gdc.voice.session.start.total",
		metric.WithDescription("Voice raid starts"),
	)
	must(err)

	SessionStop, err = meter.Int64Counter("gdc.voice.session.stop.total",
		metric.WithDescription("Voice raid stops"),
	)
	must(err)

	MixerTickDuration, err = meter.Float64Histogram("gdc.mixer.tick.duration",
		metric.WithDescription("Mixer tick processing time"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5, 10, 20),
	)
	must(err)

	MixerPipelineLatency, err = meter.Float64Histogram("gdc.mixer.pipeline.latency",
		metric.WithDescription("End-to-end latency from fanout decode to mixer output"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 20, 40, 60, 100, 200, 500),
	)
	must(err)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
