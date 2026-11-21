//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE1_OneCallerLifecycle walks one OneCaller raid through everything that
// happens to a speaker during its life, sharing a single setup:
//
//   - E1: a caller already in the owner's channel at raid start (admitted by
//     the RequestMembers prefetch) is relayed to the speaker channel.
//   - E19: the speaker, which captures nothing in this mode, is server-deafened
//     and keeps relaying while deafened.
//   - E8: a speaker whose voice connection is dropped is rejoined by
//     onVoiceLeave → ReconnectBotChannel.
//   - E9: a speaker dragged out of its channel is returned by onVoiceMove →
//     OnBotVoiceMove → ReconnectBotChannel. Needs Move Members for the listener.
//   - E4 + E19: stopping the raid undeafens the speaker, and a restart on the
//     same manager resumes delivery without touching the gateway.
//
// The E19 middle assertion is the load-bearing one. Server deafen is the only
// lever that stops Discord forwarding RTP — self_deaf is ignored by the voice
// server (measured 2026-09-09; see internal/manager/speaker_deafen.go) — and the
// whole optimisation rests on it being receive-only. If Discord ever made
// server deafen suppress outbound too, every non-capture raid would go silent.
func TestE1_OneCallerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	stopSource := startCaller(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)

	// Child cancel so StopVoiceRaid does not cancel the test's own ctx.
	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	// Only speakers bound to a channel join the raid, and this raid binds one.
	speakerID := h.RequireSpeakers(t)[0]
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	phase(t, "E1 pre-joined caller is relayed", func(t *testing.T) {
		AssertFramesReceived(t, h.Listener, speakerID, 100, 10*time.Second)
		t.Logf("E1: %d frames received", h.Listener.Receiver.FramesReceived(speakerID))
	})

	phase(t, "E19 speaker deafened and still relaying", func(t *testing.T) {
		requireDeafenPower(t, speakerID)
		waitForDeaf(t, guildID, speakerID, true, deafWait,
			"speaker should be server-deafened during a non-capture raid")
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 100, 20*time.Second)
		t.Log("E19: speaker server-deafened and relaying")
	})

	phase(t, "E8 rejoin after voice leave", func(t *testing.T) {
		if _, ok := h.Pool.GetClientByID(speakerID); !ok {
			t.Skip("speaker not connected")
		}
		// OP4 with channel_id=null → VOICE_STATE_UPDATE on the owner bot →
		// onVoiceLeave → ReconnectBotChannel.
		leaveCtx, leaveCancel := context.WithTimeout(ctx, 5*time.Second)
		h.DisconnectSpeakerVoice(leaveCtx, guildID, speakerID)
		leaveCancel()
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 20*time.Second)
		t.Log("E8: speaker reconnected and frames resumed after voice disconnect")
	})

	phase(t, "E9 rejoin after admin move", func(t *testing.T) {
		moveCtx, moveCancel := context.WithTimeout(ctx, 5*time.Second)
		err := h.MoveSpeakerVoice(moveCtx, guildID, speakerID, h.Cfg.OwnerChannelID)
		moveCancel()
		if err != nil {
			t.Skipf("E9: move member failed (listener bot may lack Move Members permission): %v", err)
		}
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 20*time.Second)
		t.Log("E9: speaker reconnected and frames resumed after voice move")
	})

	phase(t, "E4 stop", func(t *testing.T) {
		if err := mgr.StopVoiceRaid(ctx, guildID); err != nil {
			t.Fatalf("StopVoiceRaid: %v", err)
		}
	})

	phase(t, "E19 undeafened after stop", func(t *testing.T) {
		requireDeafenPower(t, speakerID)
		waitForDeaf(t, guildID, speakerID, false, deafWait,
			"speaker should be undeafened by raid teardown")
	})

	phase(t, "E4 restart resumes audio", func(t *testing.T) {
		_, restartCancel := context.WithCancel(ctx)
		if _, err := mgr.StartVoiceRaid(ctx, guildID, restartCancel, guild.RaidModeOneCaller); err != nil {
			t.Fatalf("StartVoiceRaid (restart): %v", err)
		}
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 15*time.Second)
		t.Log("E4: frames resumed after raid restart")
	})
}

// TestE2_GuildCallerMixMinus verifies that cross-channel audio is relayed:
// the listener in Speaker1ChannelID hears Speaker1 playing audio captured
// from Speaker2ChannelID (mix-minus routing excludes same-channel audio).
func TestE2_GuildCallerMixMinus(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stopSource1 := startCaller(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := startCaller(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListener)

	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Log("E2 passed: cross-channel relay confirmed via speaker bot frames in ch1")
}

// TestE6_OneManyStarTopology verifies the star-topology mode: the owner bot mixes
// audio captured from all speaker channels and plays it in the owner channel.
func TestE6_OneManyStarTopology(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	stopSource1 := startCaller(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := startCaller(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneManyGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListenerOwner := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID)

	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListenerOwner)

	// Star topology attributes the mixed output to the owner bot's ID.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Log("E6 passed: star topology — owner channel receives mixed audio from both sources")
}
