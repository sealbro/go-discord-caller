package manager

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// watchVoiceReady re-arms a bot's audio pipeline after disgo transparently
// re-identifies its voice gateway.
//
// disgo auto-reconnects the voice gateway for every close code marked
// Reconnect (voice/gateway.go: `if g.config.AutoReconnect && reconnect`).
// Code 1006 "unexpected EOF" is by far the most common one in production.
// When the resume is rejected and the gateway has to re-identify, Discord
// replies with a fresh Ready and connImpl.handleMessage opens a NEW UDP
// socket — without closing the old one. Two things break:
//
//   - The AudioReceiver goroutine stays blocked in ReadPacket on the ORPHANED
//     socket. It captured `conn := u.conn` before the swap, and that socket is
//     still open, so the read never returns an error — it simply never yields
//     another packet. Capture is dead for good.
//   - The AudioSender may already have killed itself: its handleErr treats a
//     single net.ErrClosed write as fatal and cancels the send loop forever.
//
// None of this emits a VOICE_STATE_UPDATE, because the bot never actually
// leaves the channel. onVoiceLeave / onVoiceMove therefore never fire and
// ReconnectBotChannel is never reached: the bot looks perfectly connected and
// is silent until someone drags it out of the channel and back in.
//
// Re-applying the stored reconnectApplier calls SetOpusFrameProvider and
// SetOpusFrameReceiver, which build a fresh AudioSender/AudioReceiver bound to
// the CURRENT socket — the same repair ReconnectBotChannel performs, minus the
// rejoin. The mixer graph is untouched: the applier reuses the original chOut
// channel and FanoutHandle, so no re-install is needed.
//
// Only Ready is watched, not Resumed: a successful resume keeps the existing
// UDP socket and needs no repair.
func (m *Service) watchVoiceReady(guildID, botUserID snowflake.ID, conn voice.Conn) {
	conn.SetEventHandlerFunc(func(_ voice.Gateway, op voice.Opcode, _ int, _ voice.GatewayMessageData) {
		if op != voice.OpcodeReady {
			return
		}
		// This runs inline on the voice gateway's listen goroutine (see the
		// eventHandlerFunc call at the bottom of gatewayImpl.listen), so it must
		// never block: anything slow here stalls the whole voice gateway.
		go m.reapplyAfterVoiceReady(guildID, botUserID, conn)
	})
}

// reapplyAfterVoiceReady rebuilds the provider/receiver pair for a conn whose
// voice gateway just re-identified. See watchVoiceReady for why this is needed.
//
// The initial Ready of a connection is not observed here: watchVoiceReady is
// installed after conn.Open has already returned, which happens only once the
// SessionDescription following that first Ready has arrived. Every Ready this
// handler sees is therefore a re-identify.
func (m *Service) reapplyAfterVoiceReady(guildID, botUserID snowflake.ID, conn voice.Conn) {
	// Share the in-flight guard with ReconnectBotChannel. A full rejoin already
	// re-applies the pipeline, and the two must not race on the same conn.
	if !m.reconnect.tryLock(guildID, botUserID) {
		return
	}
	defer m.reconnect.unlock(guildID, botUserID)

	if !m.HasActiveSession(guildID) {
		return
	}
	applier, ok := m.reconnect.loadApplier(guildID, botUserID)
	if !ok {
		return
	}

	// context.Background() rather than a timeout context: the applier captures
	// the ctx in its FrameDropper/metric recorders for the remaining lifetime of
	// the provider and receiver, so a ctx that gets cancelled on return would
	// silently disable their metrics. The applier itself only does local wiring
	// (no network I/O), so there is nothing to time out.
	ctx := context.Background()
	applier(ctx, conn)

	slog.InfoContext(ctx, "voice: re-armed audio pipeline after gateway re-identify",
		slog.String("guildID", guildID.String()),
		slog.String("botUserID", botUserID.String()),
	)
}
