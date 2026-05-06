//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE1_OneCaller verifies that a source bot speaking in the owner's channel
// is relayed to the speaker channel where the listener sits.
func TestE1_OneCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Source joins before StartVoiceRaid so prefetchChannelMembers captures it
	// with full RoleIDs via the RequestMembers gateway op.
	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E1 passed: %d frames received", h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}

// TestE2_GuildCallerMixMinus verifies that cross-channel audio is relayed:
// the listener in Speaker1ChannelID hears Speaker1 playing audio captured
// from Speaker2ChannelID (mix-minus routing excludes same-channel audio).
func TestE2_GuildCallerMixMinus(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	// Assign speaker 1 → channel 1, speaker 2 → channel 2.
	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	// Listener sits in channel 1.
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListener)

	// Speaker1 (in ch1) plays cross-channel audio from ch2 — verifies relay works.
	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Log("E2 passed: cross-channel relay confirmed via speaker bot frames in ch1")
}

// TestE3_PauseResume verifies that calling UpdateMixerPause toggles the mixer
// off and on, and that the listener observes a frame gap then a resumption.
func TestE3_PauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	// Wait for steady-state audio flow before pausing.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	mgr.UpdateMixerPause(h.Cfg.GuildID) // pause

	AssertFrameGap(t, h.Listener, speakerIDs[0], 2*time.Second, 5*time.Second, func() {
		mgr.UpdateMixerPause(h.Cfg.GuildID) // resume
	})
	t.Log("E3 passed: pause/resume state machine works")
}

// TestE4_RaidRestart verifies that stopping and restarting a voice raid resumes
// audio delivery. This exercises the voice-connection teardown and re-setup path
// without touching the gateway (no Discord IDENTIFY needed, no rate-limit risk).
func TestE4_RaidRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	// Use a child cancel so StopVoiceRaid doesn't cancel the test's ctx itself.
	_, session1Cancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, session1Cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	// Establish baseline — raid is running and frames are flowing.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E4: baseline established, stopping raid...")

	// Stop the raid — all speaker voice connections are torn down.
	if err := mgr.StopVoiceRaid(ctx, h.Cfg.GuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	// Brief pause to let voice connections close.
	time.Sleep(500 * time.Millisecond)

	// Restart the raid with a fresh session cancel.
	_, session2Cancel := context.WithCancel(ctx)
	if _, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, session2Cancel, guild.RaidModeOneCaller); err != nil {
		t.Fatalf("StartVoiceRaid (restart): %v", err)
	}
	t.Log("E4: raid restarted, waiting for frames to resume...")

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 15*time.Second)
	t.Log("E4 passed: frames resumed after raid restart")
}

// TestE6_OneManyStarTopology verifies the star-topology mode: the owner bot mixes
// audio captured from all speaker channels and plays it in the owner channel.
func TestE6_OneManyStarTopology(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	// Speaker1 → channel1, Speaker2 → channel2.
	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneManyGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	// Listener in the owner's channel — the owner bot plays the mixed audio there.
	stopListenerOwner := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID)

	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListenerOwner)

	// In star topology, the owner bot mixes all speaker-channel sources and plays
	// the result in OwnerChannelID. Frames arrive attributed to the owner bot's ID.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Log("E6 passed: star topology — owner channel receives mixed audio from both sources")
}

// TestE7_RequestMembersPreFetch verifies that a source bot already in the
// owner's voice channel before the raid starts is allowed by the role filter
// (caught by the RequestMembers gateway op issued at session start).
func TestE7_RequestMembersPreFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Source bot joins the owner's channel BEFORE StartVoiceRaid.
	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	// Brief pause so the voice-state update reaches the gateway before we start.
	time.Sleep(1 * time.Second)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E7 passed: pre-joined source bot allowed via RequestMembers cache pre-fetch")
}
