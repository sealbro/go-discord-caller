package telemetry

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
)

// GuildMetrics bundles every per-guild metric handle the voice pipeline needs.
// Build once via Metrics.ForGuild and pass it down — eliminates the repeated
// `Opus.For(guildID.String()).WithDrop(Session.FrameDropper(ctx, guildID, path))`
// chain that otherwise appears at every call site.
type GuildMetrics struct {
	Opus    OpusRecorder
	session *SessionMetrics
	guildID snowflake.ID
	ctx     context.Context
}

// ForGuild returns a GuildMetrics with guildID baked in. ctx is captured for
// FrameDropper construction; callers that need a different context can use
// WithCtx to derive a copy without rebuilding the OpusRecorder.
func (m *Metrics) ForGuild(ctx context.Context, guildID snowflake.ID) GuildMetrics {
	return GuildMetrics{
		Opus:    m.Opus.For(guildID.String()),
		session: &m.Session,
		guildID: guildID,
		ctx:     ctx,
	}
}

// WithCtx returns a copy of g with ctx replaced. Used inside reconnect
// appliers where the original session ctx may be stale.
func (g GuildMetrics) WithCtx(ctx context.Context) GuildMetrics {
	g.ctx = ctx
	return g
}

// GuildID exposes the baked-in snowflake for callers that still need the raw ID
// (e.g. SessionStarted/Stopped, BroadcastFromGuild).
func (g GuildMetrics) GuildID() snowflake.ID { return g.guildID }

// Session returns the underlying *SessionMetrics for cases that need
// SessionStarted/Stopped or a custom DropPath.
func (g GuildMetrics) Session() *SessionMetrics { return g.session }

// Provider returns an OpusRecorder pre-wired with the provider drop counter.
func (g GuildMetrics) Provider() OpusRecorder {
	return g.Opus.WithDrop(g.session.FrameDropper(g.ctx, g.guildID, DropPathProvider))
}

// Receiver returns an OpusRecorder pre-wired with the receiver drop counter.
func (g GuildMetrics) Receiver() OpusRecorder {
	return g.Opus.WithDrop(g.session.FrameDropper(g.ctx, g.guildID, DropPathReceiver))
}

// Drop returns a counter func for the given pipeline stage.
func (g GuildMetrics) Drop(path DropPath) func() {
	return g.session.FrameDropper(g.ctx, g.guildID, path)
}

// SessionStarted records a raid start using the bundled guild ID.
func (g GuildMetrics) SessionStarted(speakerCount int) {
	g.session.SessionStarted(g.ctx, g.guildID, speakerCount)
}

// SessionStopped records a raid stop using the bundled guild ID.
func (g GuildMetrics) SessionStopped() {
	g.session.SessionStopped(g.ctx, g.guildID)
}

// RouteTransition records one auto-router source-mode transition using the
// bundled guild ID. Used by the auto-router every time a source flips
// off/copy/mix.
func (g GuildMetrics) RouteTransition(from, to string) {
	g.session.RouteTransition(g.ctx, g.guildID, from, to)
}
