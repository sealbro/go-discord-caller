package manager

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// OnBotVoiceMove is called when a bot is moved to a different voice channel.
// It reconnects the bot to its bound channel if the new channel differs, so that
// bots cannot be permanently displaced during an active session.
// No-op when there is no active session or the bot is already in its bound channel.
func (m *Service) OnBotVoiceMove(ctx context.Context, guildID, botUserID snowflake.ID, currentChannelID *snowflake.ID) {
	if !m.HasActiveSession(guildID) {
		return
	}
	boundChID, ok := m.store.GetBoundChannel(guildID, botUserID)
	if !ok {
		return
	}
	if currentChannelID != nil && *currentChannelID == boundChID {
		return // already in the right channel
	}
	go m.ReconnectBotChannel(ctx, guildID, botUserID)
}

// ReconnectBotChannel reconnects a bot to its bound voice channel in the given guild.
// Called when a bot's voice connection drops or is moved away during an active session.
// No-op if there is no active session, the bot has no bound channel, or a reconnect
// for this bot is already in flight (prevents the leave→reconnect→leave→... loop).
func (m *Service) ReconnectBotChannel(ctx context.Context, guildID, botUserID snowflake.ID) {
	// Guard: one reconnect attempt per (guild, bot) at a time.
	// Calling Leave below fires another GuildVoiceLeave which would re-enter here;
	// tryLock makes that second call a no-op.
	if !m.reconnect.tryLock(guildID, botUserID) {
		return
	}
	defer m.reconnect.unlock(guildID, botUserID)

	if !m.HasActiveSession(guildID) {
		return
	}
	channelID, ok := m.store.GetBoundChannel(guildID, botUserID)
	if !ok || channelID == 0 {
		return
	}
	var gv pool.GuildVoice
	if botUserID == m.ownerBotID {
		gv = m.ownerVoice(guildID)
	} else {
		var found bool
		gv, found = m.speakerVoice(guildID, botUserID)
		if !found {
			return
		}
	}

	// Close the existing (possibly broken) voice connection so conn.Open starts
	// fresh instead of re-using stale internal state that causes a timeout.
	leaveCtx, leaveCancel := context.WithTimeout(ctx, voiceLeaveTimeout)
	gv.Leave(leaveCtx, guildID)
	leaveCancel()

	if !m.HasActiveSession(guildID) {
		return // session ended while we were closing
	}

	reconnCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := gv.Join(reconnCtx, guildID)
	if err != nil {
		// Single retry after a short backoff to handle transient failures
		// (Discord rate limits, brief network interruptions). The reconnect
		// guard stays held so a concurrent leave event does not race us.
		slog.WarnContext(ctx, "reconnect: first join attempt failed, retrying in 2s",
			slog.String("guildID", guildID.String()),
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		t := time.NewTimer(2 * time.Second)
		select {
		case <-reconnCtx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !m.HasActiveSession(guildID) {
			return // session ended during backoff
		}
		conn, err = gv.Join(reconnCtx, guildID)
		if err != nil {
			slog.WarnContext(ctx, "reconnect: failed to rejoin bound channel",
				slog.String("guildID", guildID.String()),
				slog.String("botUserID", botUserID.String()),
				slog.String("channelID", channelID.String()),
				slog.Any("err", err),
			)
			return
		}
	}
	// Re-apply voice provider/receiver to the new conn so audio flows again.
	// Pass ctx (the reconnect context) so the applier's FrameDroppers use a live,
	// uncancelled context rather than the stale session-start context.
	if applier, ok := m.reconnect.loadApplier(guildID, botUserID); ok {
		applier(ctx, conn)
	}
	slog.InfoContext(ctx, "reconnect: bot rejoined bound channel",
		slog.String("guildID", guildID.String()),
		slog.String("botUserID", botUserID.String()),
		slog.String("channelID", channelID.String()),
	)
}

// storeApplier saves a reconnectApplier for the given guild+bot pair.
func (m *Service) storeApplier(guildID, botUserID snowflake.ID, a reconnectApplier) {
	m.reconnect.storeApplier(guildID, botUserID, a)
}

// clearAppliers removes all reconnect appliers for a guild (call on session teardown).
func (m *Service) clearAppliers(guildID snowflake.ID) {
	m.reconnect.clearAppliers(guildID, m.poolSvc.GetIDs(), m.ownerBotID)
}

// buildApplier returns a reconnectApplier that re-wires provider/receiver on a fresh conn.
//
//   - chOut nil:     empty provider (owner-as-listener mode).
//   - chCapture nil: empty receiver (no audio capture).
//   - handle non-nil: the new receiver dispatches via the same FanoutHandle as
//     the original, so the topology wired at session start keeps receiving
//     frames after reconnect — no re-install required.
//   - botID is a test bot: file provider plays a fixed DCA loop.
//
// FrameDropper is created lazily inside the closure using the call-time ctx so
// metrics never attach to a stale session-start span.
func (m *Service) buildApplier(guildID, botID snowflake.ID, chOut <-chan []byte, chCapture chan []byte, handle *opus.FanoutHandle, allowUser func(snowflake.ID) bool) reconnectApplier {
	return func(ctx context.Context, conn voice.Conn) {
		gm := m.metrics.ForGuild(ctx, guildID)
		var provider voice.OpusFrameProvider
		switch {
		case chOut != nil:
			provider = opus.NewVoiceProvider(chOut, gm.Provider())
		default:
			provider = opus.NewEmptyVoiceProvider()
		}
		conn.SetOpusFrameProvider(provider)

		var receiver voice.OpusFrameReceiver = opus.NewEmptyVoiceReceiver()
		if chCapture != nil {
			receiver = opus.NewVoiceReceiver(chCapture, botID, allowUser, gm.Receiver(), handle)
		}
		conn.SetOpusFrameReceiver(receiver)
	}
}
