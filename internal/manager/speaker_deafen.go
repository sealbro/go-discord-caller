package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Server-deafening speaker bots in non-capture raid modes.
//
// A speaker bot in a non-capture mode (RaidMode.WithCapture() == false) never
// consumes inbound audio: VoiceConnSetup.Apply gives it an
// opus.EmptyVoiceReceiver that discards every frame. Discord does not care —
// the voice server keeps forwarding RTP for every speaking member of the
// channel, and disgo's udpConnImpl.ReadPacket transport-decrypts AND
// DAVE-decrypts each packet *before* the receiver is consulted. That is 50
// wasted decrypts per second per speaking human, per speaker bot, and it
// scales with how busy the destination channel is.
//
// Measured against a live guild on 2026-09-09, one source bot playing
// continuously into a channel with one listener bot:
//
//	self_deaf flipped on a live connection   50.0/s -> 50.0/s  (no effect)
//	self_deaf set at handshake (conn.Open)   50.0/s            (no effect)
//	server deafen (PATCH guild member)       50.0/s -> 0.0/s
//	server deafen, measured on the SENDER    50.0/s -> 50.0/s  (still sends)
//
// So self_deaf is a client-side flag the voice server ignores for forwarding,
// and only server deafen actually stops the stream. Crucially it is
// receive-only: a server-deafened bot keeps sending at full rate, which is
// what makes it usable for speaker bots at all.
//
// The catch is permissions. Server deafen is a moderation action, so the owner
// bot needs DEAFEN_MEMBERS (bit 23) in the guild AND a role above every speaker
// bot's role; without both, Discord answers 50013 Missing Permissions. Guilds
// that installed the bot before that permission was added to the invite link
// simply do not have it, so this must degrade quietly rather than fail a raid —
// see reconcileSpeakerDeaf.
//
// Two further consequences drive the design below:
//
//   - The flag persists on the guild member across voice sessions, and a
//     deafened bot cannot capture. A crash mid-session would therefore leave a
//     speaker silently unable to capture in the *next* raid. Every join
//     reconciles the flag to what the mode requires rather than assuming a
//     clean slate — that reconcile is a correctness requirement, not a tidiness
//     one.
//   - It is a moderation action: each real change costs a REST call and a guild
//     audit-log entry. ensureSpeakerDeaf skips the call when the cached voice
//     state already agrees, which keeps the common capture-mode reconcile free.

// ensureSpeakerDeaf drives speakerID's server-deaf flag in guildID to want.
//
// The cached voice state is authoritative for the server-deaf bit and is
// refreshed by the VOICE_STATE_UPDATE that the speaker's own join produces, so
// a hit is trustworthy. A miss falls through to the REST call rather than
// assuming anything — being wrong in the "already undeafened" direction would
// silently break capture.
func (m *Service) ensureSpeakerDeaf(ctx context.Context, guildID, speakerID snowflake.ID, want bool) error {
	if vs, ok := m.ownerClient.Caches.VoiceState(guildID, speakerID); ok && vs.GuildDeaf == want {
		return nil
	}
	if _, err := m.ownerClient.Rest.UpdateMember(guildID, speakerID,
		discord.MemberUpdate{Deaf: &want}, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("set server deaf=%t for speaker %s: %w", want, speakerID, err)
	}
	return nil
}

// reconcileSpeakerDeaf brings a freshly joined speaker's server-deaf flag in
// line with what the raid mode needs, and returns the undoing func to hang on
// the SpeakerResult (nil when there is nothing to undo).
//
// Non-capture modes deafen: the bot discards inbound audio anyway, so this
// just stops Discord sending it. Capture modes undeafen, which is what repairs
// a flag stranded by a previous non-capture session that never tore down
// cleanly — without it a speaker would join, look healthy, and capture silence.
//
// Failures are logged and swallowed. Deafening is an optimisation, and a raid
// that refuses to start because a PATCH was rate-limited would trade a large
// win for a much worse failure mode. The reconcile direction is the one that
// matters, and a capture-mode failure there surfaces as the existing
// "no audio from speaker" symptom rather than something new.
func (m *Service) reconcileSpeakerDeaf(ctx context.Context, guildID, speakerID snowflake.ID, withCapture bool) func(context.Context) {
	if withCapture {
		if err := m.ensureSpeakerDeaf(ctx, guildID, speakerID, false); err != nil {
			slog.WarnContext(ctx, "failed to clear speaker server-deaf; capture may be silent",
				slog.String("guildID", guildID.String()),
				slog.String("speakerID", speakerID.String()),
				slog.Any("err", err))
		}
		return nil
	}

	if err := m.ensureSpeakerDeaf(ctx, guildID, speakerID, true); err != nil {
		slog.WarnContext(ctx, "failed to server-deafen speaker; it will keep decrypting unused audio",
			slog.String("guildID", guildID.String()),
			slog.String("speakerID", speakerID.String()),
			slog.Any("err", err))
		return nil
	}

	return func(cleanupCtx context.Context) {
		if err := m.ensureSpeakerDeaf(cleanupCtx, guildID, speakerID, false); err != nil {
			slog.WarnContext(cleanupCtx, "failed to undeafen speaker on teardown; next capture raid will repair it",
				slog.String("guildID", guildID.String()),
				slog.String("speakerID", speakerID.String()),
				slog.Any("err", err))
		}
	}
}
