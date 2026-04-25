package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// BotMetrics tracks Discord entity info, bot presence, voice caller counts,
// and slash command observability.
type BotMetrics struct {
	meter        metric.Meter // retained for RegisterBotOnline
	guildInfo    metric.Int64Gauge
	botOnline    metric.Int64ObservableGauge
	voiceCallers metric.Int64UpDownCounter
	cmdCount     metric.Int64Counter
	cmdDuration  metric.Float64Histogram
}

func (b *BotMetrics) init(meter metric.Meter) (err error) {
	b.meter = meter
	if b.guildInfo, err = meter.Int64Gauge("gdc.discord.guild",
		metric.WithDescription("Known guilds; always 1. Labels: guild_id, guild_name."),
	); err != nil {
		return
	}
	if b.botOnline, err = meter.Int64ObservableGauge("gdc.bot.online",
		metric.WithDescription("1 when the bot is a registered member of the guild, absent otherwise."),
	); err != nil {
		return
	}
	if b.voiceCallers, err = meter.Int64UpDownCounter("gdc.voice.callers",
		metric.WithDescription("Number of users with the caller role currently in a voice channel, per guild."),
	); err != nil {
		return
	}
	if b.cmdCount, err = meter.Int64Counter("gdc.command.total",
		metric.WithDescription("Slash command invocations"),
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

// RecordGuildInfo emits the guild info gauge for a known guild.
func (b *BotMetrics) RecordGuildInfo(ctx context.Context, guildID, guildName string) {
	b.guildInfo.Record(ctx, 1,
		metric.WithAttributes(
			attribute.String("guild_id", guildID),
			attribute.String("guild_name", guildName),
		),
	)
}

// VoiceCallerAdd adjusts the voice caller counter for a guild/channel by delta.
func (b *BotMetrics) VoiceCallerAdd(ctx context.Context, delta int64, guildID, channelID string) {
	b.voiceCallers.Add(ctx, delta,
		metric.WithAttributes(
			attribute.String("guild_id", guildID),
			attribute.String("channel_id", channelID),
		),
	)
}

// RecordCommand records slash command count and duration for a command/guild pair.
func (b *BotMetrics) RecordCommand(ctx context.Context, command, guildID string, durationSeconds float64) {
	attrs := metric.WithAttributes(
		attribute.String("command", command),
		attribute.String("guild.id", guildID),
	)
	b.cmdCount.Add(ctx, 1, attrs)
	b.cmdDuration.Record(ctx, durationSeconds, attrs)
}
