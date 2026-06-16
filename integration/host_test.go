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

	// Source must be in-channel before raid start so prefetchChannelMembers
	// resolves it via RequestMembers with full RoleIDs.
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

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListener)

	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Log("E2 passed: cross-channel relay confirmed via speaker bot frames in ch1")
}

// TestE4_RaidRestart verifies that stopping and restarting a voice raid resumes
// audio delivery. This exercises the voice-connection teardown and re-setup path
// without touching the gateway (no Discord IDENTIFY needed, no rate-limit risk).
func TestE4_RaidRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	// Child cancel so StopVoiceRaid does not cancel the test's own ctx.
	_, session1Cancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, session1Cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)
	t.Log("E4: baseline established, stopping raid...")

	if err := mgr.StopVoiceRaid(ctx, h.Cfg.GuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

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

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneManyGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListenerOwner := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID)

	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListenerOwner)

	// Star topology attributes the mixed output to the owner bot's ID.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Log("E6 passed: star topology — owner channel receives mixed audio from both sources")
}

// TestE7_RequestMembersPreFetch verifies that a source bot already in the
// owner's voice channel before the raid starts is allowed by the role filter
// (caught by the RequestMembers gateway op issued at session start).
func TestE7_RequestMembersPreFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	// Let the VOICE_STATE_UPDATE land before we kick off the raid.
	time.Sleep(1 * time.Second)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E7 passed: pre-joined source bot allowed via RequestMembers cache pre-fetch")
}
