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

// Speaker pool metrics
var (
	SpeakerJoins metric.Int64Counter
)

// Voice frame metrics
var (
	VoiceFramesReceived metric.Int64Counter
	VoiceFramesDropped  metric.Int64Counter
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

	SpeakerJoins, err = meter.Int64Counter("gdc.voice.speaker.joined.total",
		metric.WithDescription("Speaker bot voice channel joins"),
	)
	must(err)

	VoiceFramesReceived, err = meter.Int64Counter("gdc.voice.frames.received.total",
		metric.WithDescription("Opus frames forwarded to relay channel"),
	)
	must(err)

	VoiceFramesDropped, err = meter.Int64Counter("gdc.voice.frames.dropped.total",
		metric.WithDescription("Opus frames dropped"),
	)
	must(err)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
