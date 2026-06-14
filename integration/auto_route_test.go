//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE13_AutoRouteCopyToMixTransition verifies the auto-router switches a
// source from copy mode to mix mode when a second caller joins its channel,
// and that audio delivery continues across the transition without a
// catastrophic gap. The voice-event handlers fire onVoiceJoin → AutoRoute →
// the router debounces (250 ms) → Recompute → install spec swap.
//
// Frame-loss budget: the transition window is bounded by (a) the debounce
// (250 ms) plus (b) the install swap itself, which is atomic via the
// FanoutHandle.state pointer. Steady-state delivery is 50 frames/s; we
// tolerate up to 1 second of gap (≤ 50 frames) before flagging a regression.
func TestE13_AutoRouteCopyToMixTransition(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need second speaker to trigger copy→mix")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Only the first speaker is in the owner channel at raid start →
	// router starts the owner source in copy mode.
	stopSpeaker1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)

	var stopSpeaker2 func() = func() {}
	t.Cleanup(func() {
		stopSpeaker1()
		stopSpeaker2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	// Establish steady-state delivery in copy mode.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E13: copy-mode steady state established, joining second caller...")

	// Second caller joins → onVoiceJoin → AutoRoute → router cascade flips
	// the owner source to mix mode (C=2). Install swap re-wires the
	// FanoutHandle to per-user SourceBuffers.
	stop2, err := h.Speaker2.StartPlaying(ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("Speaker2.StartPlaying: %v", err)
	}
	stopSpeaker2 = stop2

	// Frame delivery must continue. We allow a brief gap for the debounce +
	// install swap (≤ 1 s) but expect a healthy stream within 5 s.
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E13 passed: copy→mix transition kept audio flowing")
}

// TestE14_AutoRouteMixToCopyTransition is the reverse of E13: starts with
// two callers (mix mode), drops one, verifies the router transitions back
// to copy mode without losing delivery. Validates the install teardown +
// mixer SetPaused path on the demotion.
func TestE14_AutoRouteMixToCopyTransition(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need second speaker to start in mix mode")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Both speakers in the owner channel at raid start → router starts the
	// owner source in mix mode (C=2).
	stopSpeaker1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stopSpeaker2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	t.Cleanup(func() {
		stopSpeaker1()
		stopSpeaker2() // safe to call twice; cleanup is idempotent
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	// Establish steady-state delivery in mix mode.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E14: mix-mode steady state established, dropping second caller...")

	// Drop caller 2 → onVoiceLeave → AutoRoute → cascade drops C from 2 to
	// 1 → router transitions owner source back to copy mode. Teardown
	// closure must RemoveInput the per-user SBs from the destination
	// mixer (still drained by the router on the demotion).
	stopSpeaker2()
	stopSpeaker2 = func() {}

	// Frame delivery continues from the remaining caller via the copy path.
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E14 passed: mix→copy transition kept audio flowing")
}

// TestE15_TwoCallersSameChannelNoEviction exercises the §4.3 fix: when two
// users in the same source channel both speak, each gets their own
// SourceBuffer per destination mixer so the cap-3 ring does not silently
// evict one user's frames. Before the per-user keying change a shared
// SourceBuffer would drop frames continually under steady stereo load; the
// observable symptom was a degraded (but not absent) listener stream.
//
// We can't easily distinguish "both audios audibly mixed" from "only one"
// without spectrogram analysis, so this test verifies the stronger
// invariant: the destination mixer continues producing frames at the
// expected rate during sustained two-caller speech, and the pipeline does
// not panic.
func TestE15_TwoCallersSameChannelNoEviction(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need both speakers in the same channel")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Both speakers in the owner channel from the start (mix mode).
	stopSpeaker1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stopSpeaker2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	t.Cleanup(func() {
		stopSpeaker1()
		stopSpeaker2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	// Establish steady-state at the destination.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E15: mix-mode steady state established with both callers; verifying sustained throughput...")

	// Verify sustained delivery: at least 400 additional frames within 10 s
	// (≈ 80 % of nominal 50 frames/s). The per-user keying fix prevents the
	// shared-ring eviction that previously degraded throughput when two
	// speakers in one channel both produce frames per Opus tick.
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 400, 10*time.Second)
	t.Log("E15 passed: two-callers-in-one-channel mix kept throughput at expected rate")
}
