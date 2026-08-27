package telemetry

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// SessionMetrics tracks voice raid session lifecycle and fanout health.
type SessionMetrics struct {
	active           metric.Int64UpDownCounter
	starts           metric.Int64Counter
	stops            metric.Int64Counter
	speakers         metric.Int64Gauge
	mode             metric.Int64Gauge
	dropped          metric.Int64Counter
	routeTransitions metric.Int64Counter
}

func (s *SessionMetrics) init(meter metric.Meter) (err error) {
	if s.active, err = meter.Int64UpDownCounter("gdc.voice.sessions.active",
		metric.WithDescription("Currently active voice raid sessions"),
	); err != nil {
		return
	}
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
		metric.WithDescription("1 while a guild's active voice raid session is using the labelled mode, 0 once that session ends. Labels: guild_id, mode."),
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
	return nil
}

// SessionStarted records the start of a voice raid: increments active counter,
// start total, sets the speaker gauge for guildID, and flags mode as the
// session's active mode.
func (s *SessionMetrics) SessionStarted(ctx context.Context, guildID snowflake.ID, speakerCount int, mode string) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.active.Add(ctx, 1, attrs)
	s.starts.Add(ctx, 1, attrs)
	s.speakers.Record(ctx, int64(speakerCount), attrs)
	s.mode.Record(ctx, 1, metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("mode", mode),
	))
}

// SessionStopped records the end of a voice raid: resets speaker gauge to 0,
// clears the mode gauge for mode (the mode the ending session was started
// with), decrements active counter, and increments stop total.
func (s *SessionMetrics) SessionStopped(ctx context.Context, guildID snowflake.ID, mode string) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.speakers.Record(ctx, 0, attrs)
	s.active.Add(ctx, -1, attrs)
	s.stops.Add(ctx, 1, attrs)
	s.mode.Record(ctx, 0, metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("mode", mode),
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
	opt := metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("path", string(path)),
	)
	return func() { s.dropped.Add(context.Background(), 1, opt) }
}
