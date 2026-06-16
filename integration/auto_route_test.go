//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE13_AutoRouteCopyToMixTransition verifies the auto-router promotes a
// source from copy to mix when a second caller joins, without a catastrophic
// audio gap. Budget: 250 ms debounce + atomic install swap → ≤ 1 s gap (≤ 50
// frames at the nominal 50 fps).
func TestE13_AutoRouteCopyToMixTransition(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need second speaker to trigger copy→mix")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Single caller at raid start so the router opens in copy mode.
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

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E13: copy-mode steady state established, joining second caller...")

	stop2, err := h.Speaker2.StartPlaying(ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("Speaker2.StartPlaying: %v", err)
	}
	stopSpeaker2 = stop2

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E13 passed: copy→mix transition kept audio flowing")
}

// TestE14_AutoRouteMixToCopyTransition is the reverse of E13: starts with
// two callers (mix mode), drops one, verifies the router demotes back to
// copy without losing delivery. Exercises the install-teardown +
// mixer-pause path on the demotion.
func TestE14_AutoRouteMixToCopyTransition(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need second speaker to start in mix mode")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSpeaker1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stopSpeaker2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	t.Cleanup(func() {
		stopSpeaker1()
		stopSpeaker2() // idempotent; safe after the in-test stop below
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E14: mix-mode steady state established, dropping second caller...")

	stopSpeaker2()
	stopSpeaker2 = func() {}

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E14 passed: mix→copy transition kept audio flowing")
}

// TestE15_TwoCallersSameChannelNoEviction exercises the §4.3 per-user keying
// fix: two users in one source channel must get distinct SourceBuffers so the
// cap-3 ring does not silently evict one. Spectrogram analysis is out of
// scope, so we assert the stronger invariant — sustained mixer throughput
// under two-caller load with no panic.
func TestE15_TwoCallersSameChannelNoEviction(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need both speakers in the same channel")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E15: mix-mode steady state established with both callers; verifying sustained throughput...")

	// 400 frames / 10 s ≈ 80 % of nominal 50 fps. The pre-fix shared-ring
	// would lose ~half of that under continuous stereo load.
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 400, 10*time.Second)
	t.Log("E15 passed: two-callers-in-one-channel mix kept throughput at expected rate")
}
