//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE1_OneCaller verifies that a source bot speaking in the owner's channel
// is relayed to the speaker channel where the listener sits.
func TestE1_OneCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Source joins before StartVoiceRaid so prefetchChannelMembers captures it
	// with full RoleIDs via the RequestMembers gateway op.
	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	raidCode, err := mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	t.Logf("raid started, relay code: %s", raidCode)

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E1 passed: %d frames received", h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}

// TestE2_GuildCallerMixMinus verifies that cross-channel audio is relayed:
// the listener in Speaker1ChannelID hears Speaker1 playing audio captured
// from Speaker2ChannelID (mix-minus routing excludes same-channel audio).
func TestE2_GuildCallerMixMinus(t *testing.T) {
	skipIfMissing(t, h.cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source1.StartPlaying: %v", err)
	}
	stopSource2, err := h.Speaker2.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker2ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source2.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Assign speaker 1 → channel 1, speaker 2 → channel 2.
	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID, h.cfg.Speaker2ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	// Listener sits in channel 1.
	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource1()
		stopSource2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Speaker1 (in ch1) plays cross-channel audio from ch2 — verifies relay works.
	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Log("E2 passed: cross-channel relay confirmed via speaker bot frames in ch1")
}

// TestE3_PauseResume verifies that calling UpdateMixerPause toggles the mixer
// off and on, and that the listener observes a frame gap then a resumption.
func TestE3_PauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	speakerIDs := h.Pool.ConnectedSpeakerIDs()

	// Wait for steady-state audio flow before pausing.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	mgr.UpdateMixerPause(h.cfg.GuildID) // pause

	AssertFrameGap(t, h.Listener, speakerIDs[0], 2*time.Second, 5*time.Second, func() {
		mgr.UpdateMixerPause(h.cfg.GuildID) // resume
	})
	t.Log("E3 passed: pause/resume state machine works")
}

// TestE4_RaidRestart verifies that stopping and restarting a voice raid resumes
// audio delivery. This exercises the voice-connection teardown and re-setup path
// without touching the gateway (no Discord IDENTIFY needed, no rate-limit risk).
func TestE4_RaidRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)

	// Use a child cancel so StopVoiceRaid doesn't cancel the test's ctx itself.
	_, session1Cancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, session1Cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	if len(speakerIDs) == 0 {
		t.Skip("no speakers in pool")
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Establish baseline — raid is running and frames are flowing.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E4: baseline established, stopping raid...")

	// Stop the raid — all speaker voice connections are torn down.
	if err := mgr.StopVoiceRaid(ctx, h.cfg.GuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}

	// Brief pause to let voice connections close.
	time.Sleep(500 * time.Millisecond)

	// Restart the raid with a fresh session cancel.
	_, session2Cancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, session2Cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (restart): %v", err)
	}
	t.Log("E4: raid restarted, waiting for frames to resume...")

	baseAfterRestart := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterRestart+50 {
			t.Log("E4 passed: frames resumed after raid restart")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E4 failed: frames did not resume within 15 s after raid restart")
}

// TestE6_OneManyStarTopology verifies the star-topology mode: the owner bot mixes
// audio captured from all speaker channels and plays it in the owner channel.
func TestE6_OneManyStarTopology(t *testing.T) {
	skipIfMissing(t, h.cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source1.StartPlaying: %v", err)
	}
	stopSource2, err := h.Speaker2.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker2ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source2.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Speaker1 → channel1, Speaker2 → channel2.
	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID, h.cfg.Speaker2ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneManyGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	// Listener in the owner's channel — the owner bot plays the mixed audio there.
	stopListenerOwner, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID)
	if err != nil {
		t.Fatalf("listener (owner channel).StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource1()
		stopSource2()
		stopListenerOwner()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// In star topology, the owner bot mixes all speaker-channel sources and plays
	// the result in OwnerChannelID. Frames arrive attributed to the owner bot's ID.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Log("E6 passed: star topology — owner channel receives mixed audio from both sources")
}

// TestE7_RequestMembersPreFetch verifies that a source bot already in the
// owner's voice channel before the raid starts is allowed by the role filter
// (caught by the RequestMembers gateway op issued at session start).
func TestE7_RequestMembersPreFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Source bot joins the owner's channel BEFORE StartVoiceRaid.
	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	// Brief pause so the voice-state update reaches the gateway before we start.
	time.Sleep(1 * time.Second)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E7 passed: pre-joined source bot allowed via RequestMembers cache pre-fetch")
}
