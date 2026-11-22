package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// SessionMetrics tracks voice raid session lifecycle and fanout health.
type SessionMetrics struct {
	starts           metric.Int64Counter
	stops            metric.Int64Counter
	speakers         metric.Int64Gauge
	mode             metric.Int64Gauge
	dropped          metric.Int64Counter
	routeTransitions metric.Int64Counter
	botJoin          metric.Float64Histogram

	primed sync.Map // guild ID -> struct{}
}

func (s *SessionMetrics) init(meter metric.Meter) (err error) {
	if s.starts, err = meter.Int64Counter("gdc.voice.session.start.total",
		metric.WithDescription("Voice raid starts"),
	); err != nil {
		return
	}
	if s.stops, err = meter.Int64Counter("gdc.voice.session.stop.total",
		metric.WithDescription("Voice raid stops"),
	); err != nil {
		return
	}
	if s.speakers, err = meter.Int64Gauge("gdc.session.speakers",
		metric.WithDescription("Number of speakers (pool speaker bots plus the owner bot, if it joined) in the active voice raid session."),
	); err != nil {
		return
	}
	if s.mode, err = meter.Int64Gauge("gdc.session.mode",
		metric.WithDescription("1 while a guild's active voice raid session is using the labelled mode, 0 once that session ends; summed over mode it is the guild's active session count. Labels: guild_id, mode."),
	); err != nil {
		return
	}
	if s.dropped, err = meter.Int64Counter("gdc.fanout.frames.dropped.total",
		metric.WithDescription("Opus frames dropped due to full channels in the fanout/relay pipeline."),
	); err != nil {
		return
	}
	if s.routeTransitions, err = meter.Int64Counter("gdc.session.route_transitions.total",
		metric.WithDescription("Auto-router source-mode transitions (off/copy/mix). Labels include the from/to mode names."),
	); err != nil {
		return
	}
	if s.botJoin, err = meter.Float64Histogram("gdc.voice.bot.join.duration",
		metric.WithDescription("Time from the start of a raid until a bot is in its voice channel, or until its join gave up. Labels: guild_id, bot_kind (owner, speaker), outcome (joined, failed)."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 3, 5, 10, 15),
	); err != nil {
		return
	}
	return nil
}

// PrimeGuild exports guildID's start and stop counters at zero. A counter
// series born holding 1 has no earlier sample, so Prometheus' increase() misses
// the guild's first raid since the process started. Call it from a collection
// that runs before any raid; repeat calls are no-ops.
func (s *SessionMetrics) PrimeGuild(ctx context.Context, guildID snowflake.ID) {
	if _, seen := s.primed.LoadOrStore(guildID, struct{}{}); seen {
		return
	}
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.starts.Add(ctx, 0, attrs)
	s.stops.Add(ctx, 0, attrs)
}

// SessionStarted records the start of a voice raid: increments the start total,
// sets the speaker gauge for guildID, and flags mode as the session's active
// mode.
func (s *SessionMetrics) SessionStarted(ctx context.Context, guildID snowflake.ID, speakerCount int, mode string) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.starts.Add(ctx, 1, attrs)
	s.speakers.Record(ctx, int64(speakerCount), attrs)
	s.mode.Record(ctx, 1, metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("mode", mode),
	))
}

// SessionStopped records the end of a voice raid: resets speaker gauge to 0,
// clears the mode gauge for mode (the mode the ending session was started
// with), and increments the stop total.
func (s *SessionMetrics) SessionStopped(ctx context.Context, guildID snowflake.ID, mode string) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.speakers.Record(ctx, 0, attrs)
	s.stops.Add(ctx, 1, attrs)
	s.mode.Record(ctx, 0, metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("mode", mode),
	))
}

// BotJoined records how long a bot took, from the start of the raid, to get
// into its voice channel; a non-nil err records the join as failed.
func (s *SessionMetrics) BotJoined(guildID snowflake.ID, kind string, took time.Duration, err error) {
	outcome := "joined"
	if err != nil {
		outcome = "failed"
	}
	s.botJoin.Record(context.Background(), took.Seconds(), metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("bot_kind", kind),
		attribute.String("outcome", outcome),
	))
}

// RouteTransition records one source-mode transition (off/copy/mix) for
// guildID, labelled with the from/to mode names. Call once per source per
// install swap in the auto-router. context.Background() is used (rather than
// the session ctx) so a fast-following session-cancel does not drop the
// last transition record.
func (s *SessionMetrics) RouteTransition(_ context.Context, guildID snowflake.ID, from, to string) {
	s.routeTransitions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("from", from),
		attribute.String("to", to),
	))
}

// DropPath identifies the pipeline stage where an Opus frame was dropped.
type DropPath string

const (
	DropPathMixer        DropPath = "mixer"
	DropPathDirect       DropPath = "direct"
	DropPathChannelMixer DropPath = "channel_mixer"
	DropPathRelayBridge  DropPath = "relay_bridge"
	DropPathProvider     DropPath = "provider"
	DropPathReceiver     DropPath = "receiver"
)

// FrameDropper pre-computes the metric attributes for guildID+path and returns
// a zero-argument function that records one dropped frame.
// Call once before a hot loop and invoke the returned func on each drop — this
// avoids per-frame attribute allocations on the hot path.
// context.Background() is used deliberately: counter increments are fire-and-forget
// and must not be tied to the session span (which may be cancelled before the
// goroutine exits, causing exporters to silently drop the metric).
func (s *SessionMetrics) FrameDropper(_ context.Context, guildID snowflake.ID, path DropPath) func() {
	opts := []metric.AddOption{metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("path", string(path)),
	)}
	return func() { s.dropped.Add(context.Background(), 1, opts...) }
}

// SpeakerFrameDropper is FrameDropper for a drop on one speaker bot's output
// queue. The bot_id label tells one stuck speaker apart from every speaker
// falling behind, which a guild-wide count cannot.
func (s *SessionMetrics) SpeakerFrameDropper(guildID snowflake.ID, path DropPath, botID snowflake.ID) func() {
	opts := []metric.AddOption{metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("path", string(path)),
		attribute.String("bot_id", botID.String()),
	)}
	return func() { s.dropped.Add(context.Background(), 1, opts...) }
}
