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
	guildInfo    metric.Int64ObservableGauge
	botOnline    metric.Int64ObservableGauge
	voiceCallers metric.Int64ObservableUpDownCounter
	cmdStarted   metric.Int64Counter
	cmdCount     metric.Int64Counter
	cmdDuration  metric.Float64Histogram
}

func (b *BotMetrics) init(meter metric.Meter) (err error) {
	b.meter = meter
	if b.guildInfo, err = meter.Int64ObservableGauge("gdc.discord.guild",
		metric.WithDescription("Info gauge for known guilds; value is always 1. Labels: guild_id, guild_name."),
	); err != nil {
		return
	}
	if b.botOnline, err = meter.Int64ObservableGauge("gdc.bot.online",
		metric.WithDescription("1 when the bot is a registered member of the guild, absent otherwise."),
	); err != nil {
		return
	}
	if b.voiceCallers, err = meter.Int64ObservableUpDownCounter("gdc.voice.callers",
		metric.WithDescription("Number of users with the caller role currently in a voice channel, per guild."),
	); err != nil {
		return
	}
	if b.cmdStarted, err = meter.Int64Counter("gdc.command.started.total",
		metric.WithDescription("Slash command invocations entering the handler; exceeds gdc.command.total by the number still running."),
	); err != nil {
		return
	}
	if b.cmdCount, err = meter.Int64Counter("gdc.command.total",
		metric.WithDescription("Slash command invocations that completed"),
	); err != nil {
		return
	}
	if b.cmdDuration, err = meter.Float64Histogram("gdc.command.duration",
		metric.WithDescription("Slash command execution duration in seconds"),
		metric.WithUnit("s"),
	); err != nil {
		return
	}
	return nil
}

// RegisterBotOnline registers cb as the observable callback for the bot online gauge.
func (b *BotMetrics) RegisterBotOnline(cb metric.Callback) error {
	_, err := b.meter.RegisterCallback(cb, b.botOnline)
	return err
}

// ObserveBotOnline emits a value of 1 for the given userID/guildID pair via o.
// Call inside the callback registered with RegisterBotOnline.
func (b *BotMetrics) ObserveBotOnline(o metric.Observer, userID, guildID string) {
	o.ObserveInt64(b.botOnline, 1,
		metric.WithAttributes(
			attribute.String("user_id", userID),
			attribute.String("guild_id", guildID),
		),
	)
}

// RegisterGuildInfo registers cb as the observable callback for the guild info gauge.
// Call once after guilds are available (e.g. from StartMetrics) so the first
// collection cycle has data to emit.
func (b *BotMetrics) RegisterGuildInfo(cb metric.Callback) error {
	_, err := b.meter.RegisterCallback(cb, b.guildInfo)
	return err
}

// ObserveGuildInfo emits a value of 1 for the given guildID/guildName pair via o.
// Call inside the callback registered with RegisterGuildInfo.
func (b *BotMetrics) ObserveGuildInfo(o metric.Observer, guildID, guildName string) {
	o.ObserveInt64(b.guildInfo, 1,
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
	attrs := commandAttrs(command, guildID)
	b.cmdCount.Add(ctx, 1, attrs)
	b.cmdDuration.Record(ctx, durationSeconds, attrs)
}

func commandAttrs(command, guildID string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("command", command),
		attribute.String("guild.id", guildID),
	)
}
