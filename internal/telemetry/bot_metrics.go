package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// BotMetrics tracks Discord entity info, bot presence, voice caller counts,
// and slash command observability.
type BotMetrics struct {
	meter        metric.Meter // retained for the Register* callbacks
	guild        metric.Int64ObservableGauge
	voiceCallers metric.Int64ObservableUpDownCounter
	cmdStarted   metric.Int64Counter
	cmdDuration  metric.Float64Histogram
}

func (b *BotMetrics) init(meter metric.Meter) (err error) {
	b.meter = meter
	if b.guild, err = meter.Int64ObservableGauge("gdc.discord.guild",
		metric.WithDescription("One series per guild the owner bot is in; value is the number of bots, the owner and its speakers, registered in it. Labels: guild_id, guild_name."),
	); err != nil {
		return
	}
	if b.voiceCallers, err = meter.Int64ObservableUpDownCounter("gdc.voice.callers",
		metric.WithDescription("Number of users with the caller role currently in a voice channel of a guild with an active raid."),
	); err != nil {
		return
	}
	if b.cmdStarted, err = meter.Int64Counter("gdc.command.started.total",
		metric.WithDescription("Slash command invocations entering the handler; exceeds the gdc.command.duration count by the number still running."),
	); err != nil {
		return
	}
	if b.cmdDuration, err = meter.Float64Histogram("gdc.command.duration",
		metric.WithDescription("Slash command execution duration in seconds; its count is the number of invocations that completed."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 3, 5),
	); err != nil {
		return
	}
	return nil
}

// RegisterGuilds registers cb as the observable callback for the guild gauge.
// Call once after guilds are available (e.g. from StartMetrics) so the first
// collection cycle has data to emit.
func (b *BotMetrics) RegisterGuilds(cb metric.Callback) error {
	_, err := b.meter.RegisterCallback(cb, b.guild)
	return err
}

// ObserveGuild emits the number of bots registered in guildID via o.
// Call inside the callback registered with RegisterGuilds.
func (b *BotMetrics) ObserveGuild(o metric.Observer, guildID, guildName string, bots int) {
	o.ObserveInt64(b.guild, int64(bots),
		metric.WithAttributes(
			attribute.String("guild_id", guildID),
			attribute.String("guild_name", guildName),
		),
	)
}

// RegisterVoiceCallers registers cb as the observable callback for the voice
// callers gauge.
func (b *BotMetrics) RegisterVoiceCallers(cb metric.Callback) error {
	_, err := b.meter.RegisterCallback(cb, b.voiceCallers)
	return err
}

// ObserveVoiceCallers emits the number of callers in a guild's voice channel via o.
// Call inside the callback registered with RegisterVoiceCallers.
func (b *BotMetrics) ObserveVoiceCallers(o metric.Observer, count int64, guildID, channelID string) {
	o.ObserveInt64(b.voiceCallers, count,
		metric.WithAttributes(
			attribute.String("guild_id", guildID),
			attribute.String("channel_id", channelID),
		),
	)
}

// RecordCommandStart records a slash command entering its handler. RecordCommand
// fires only once the handler returns, so without this a handler that blocks
// forever looks identical to a command nobody ran.
func (b *BotMetrics) RecordCommandStart(ctx context.Context, command, guildID string) {
	b.cmdStarted.Add(ctx, 1, commandAttrs(command, guildID))
}

// RecordCommand records slash command completion and duration for a command/guild pair.
func (b *BotMetrics) RecordCommand(ctx context.Context, command, guildID string, durationSeconds float64) {
	b.cmdDuration.Record(ctx, durationSeconds, commandAttrs(command, guildID))
}

func commandAttrs(command, guildID string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("command", command),
		attribute.String("guild.id", guildID),
	)
}
