package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meter = otel.Meter(ServiceName)

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
	VoiceCallers   metric.Int64UpDownCounter
)

// Mixer metrics
var (
	MixerTickDuration    metric.Float64Histogram
	MixerPipelineLatency metric.Float64Histogram
)

// Discord entity info and bot presence
var (
	GuildInfo metric.Int64Gauge
	BotOnline metric.Int64ObservableGauge
)

// Pool metrics
var (
	PoolBotsTotal         metric.Int64ObservableGauge
	PoolBotsConnected     metric.Int64ObservableGauge
	PoolReconnectAttempts metric.Int64Counter
	PoolReconnectFailures metric.Int64Counter
)

// Session metrics
var (
	SessionSpeakers     metric.Int64Gauge
	FanoutFramesDropped metric.Int64Counter
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

	VoiceCallers, err = meter.Int64UpDownCounter("gdc.voice.callers",
		metric.WithDescription("Number of users with the caller role currently in a voice channel, per guild."),
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

	GuildInfo, err = meter.Int64Gauge("gdc.discord.guild",
		metric.WithDescription("Known guilds; always 1. Labels: guild_id, guild_name."),
	)
	must(err)

	BotOnline, err = meter.Int64ObservableGauge("gdc.bot.online",
		metric.WithDescription("1 when the bot is a registered member of the guild, absent otherwise."),
	)
	must(err)

	PoolBotsTotal, err = meter.Int64ObservableGauge("gdc.pool.bots.total",
		metric.WithDescription("Total speaker bots registered in the pool."),
	)
	must(err)

	PoolBotsConnected, err = meter.Int64ObservableGauge("gdc.pool.bots.connected",
		metric.WithDescription("Speaker bots with a healthy gateway connection."),
	)
	must(err)

	PoolReconnectAttempts, err = meter.Int64Counter("gdc.pool.reconnect.attempts.total",
		metric.WithDescription("Watchdog gateway reconnect attempts."),
	)
	must(err)

	PoolReconnectFailures, err = meter.Int64Counter("gdc.pool.reconnect.failures.total",
		metric.WithDescription("Watchdog gateway reconnect failures."),
	)
	must(err)

	SessionSpeakers, err = meter.Int64Gauge("gdc.session.speakers",
		metric.WithDescription("Number of speaker bots that joined the active voice raid session."),
	)
	must(err)

	FanoutFramesDropped, err = meter.Int64Counter("gdc.fanout.frames.dropped.total",
		metric.WithDescription("Opus frames dropped due to full channels in the fanout/relay pipeline."),
	)
	must(err)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
