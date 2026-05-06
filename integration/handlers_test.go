//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE8_BotReconnectAfterVoiceLeave verifies that when a speaker bot's voice
// connection is dropped during an active raid, the manager detects the departure
// via onVoiceLeave and calls ReconnectBotChannel to rejoin the bound channel,
// restoring audio delivery.
func TestE8_BotReconnectAfterVoiceLeave(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, sessionCancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, sessionCancel, guild.RaidModeOneCaller)
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

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Drop the speaker's voice connection. Leave sends OP4 (channel_id=null) to
	// Discord, which emits VOICE_STATE_UPDATE. The owner bot's onVoiceLeave handler
	// fires → bot is detected as left → ReconnectBotChannel rejoins the bound channel.
	if _, ok := h.Pool.GetClientByID(speakerIDs[0]); !ok {
		t.Skip("speaker not connected")
	}
	leaveCtx, leaveCancel := context.WithTimeout(ctx, 5*time.Second)
	h.DisconnectSpeakerVoice(leaveCtx, h.cfg.GuildID, speakerIDs[0])
	leaveCancel()
	t.Log("E8: speaker voice connection dropped, waiting for ReconnectBotChannel...")

	baseAfterDrop := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterDrop+50 {
			t.Log("E8 passed: speaker reconnected and frames resumed after voice disconnect")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E8 failed: frames did not resume within 20 s after speaker voice disconnect")
}

// TestE9_BotReconnectAfterVoiceMove verifies that when a speaker bot is moved to
// a different voice channel during an active raid (e.g. admin drag), the manager
// detects the displacement via onVoiceMove and calls ReconnectBotChannel to return
// the bot to its bound channel, restoring audio delivery.
//
// Requires the owner bot to have the Move Members permission in the test guild.
func TestE9_BotReconnectAfterVoiceMove(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, sessionCancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, sessionCancel, guild.RaidModeOneCaller)
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

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Use the owner bot's REST API to move the speaker into the owner channel,
	// displacing it from its bound Speaker1ChannelID. This fires GuildVoiceMove on
	// the owner bot → onVoiceMove → OnBotVoiceMove → ReconnectBotChannel.
	moveCtx, moveCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := h.MoveSpeakerVoice(moveCtx, h.cfg.GuildID, speakerIDs[0], h.cfg.OwnerChannelID); err != nil {
		moveCancel()
		t.Skipf("E9: move member failed (owner bot may lack Move Members permission): %v", err)
	}
	moveCancel()
	t.Log("E9: speaker moved to owner channel, waiting for ReconnectBotChannel...")

	baseAfterMove := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterMove+50 {
			t.Log("E9 passed: speaker reconnected and frames resumed after voice move")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E9 failed: frames did not resume within 20 s after speaker was moved to wrong channel")
}

// TestE10_CallerJoinAfterRaidStart verifies the onVoiceJoin live-join path:
// a caller who joins the owner channel AFTER the raid is already active is picked
// up by the handler → NotifyMemberUpdate → AllowFilter updated → their audio is
// captured and relayed. This is distinct from E1 (caller present at StartVoiceRaid,
// captured via RequestMembers prefetch) and E7 (caller pre-joined, RequestMembers).
func TestE10_CallerJoinAfterRaidStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start the raid with an empty owner channel — no RequestMembers prefetch occurs.
	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err := mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
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

	// Caller joins AFTER the raid is running. onVoiceJoin fires on the owner bot
	// → NotifyMemberUpdate updates the AllowFilter → audio is relayed.
	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlaying: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Log("E10 passed: caller joined after raid start → captured via onVoiceJoin handler")
}

// TestE11_MixerPauseResumeViaVoiceEvents verifies the auto-pause/resume path
// driven by Discord voice events rather than direct API calls (contrast with E3):
//   - onVoiceLeave (non-bot) → UpdateMixerPause → mixer pauses when last caller leaves
//   - onVoiceJoin  (non-bot) → UpdateMixerPause → mixer resumes when caller rejoins
func TestE11_MixerPauseResumeViaVoiceEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Source joins before raid so it is captured from the start.
	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, sessionCancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, sessionCancel, guild.RaidModeOneCaller)
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

	// Establish steady-state audio.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E11: baseline established, dropping caller...")

	// Caller leaves → onVoiceLeave → UpdateMixerPause → mixer pauses.
	// Caller rejoins → onVoiceJoin → UpdateMixerPause → mixer resumes.
	// AssertFrameGap captures the pause window then triggers the resume func.
	stopSource() // leave; cleanup no-ops on subsequent calls
	stopSource = func() {}

	AssertFrameGap(t, h.Listener, speakerIDs[0], 2*time.Second, 10*time.Second, func() {
		var err error
		stopSource, err = h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
		if err != nil {
			t.Errorf("speaker rejoin: %v", err)
		}
	})
	t.Log("E11 passed: mixer auto-paused on caller leave and resumed on caller rejoin via voice event handlers")
}

// TestE12_AllowFilterUpdatedOnRoleRevoke verifies the onGuildMemberUpdate path:
// removing the caller role mid-session causes NotifyMemberUpdate to update the
// AllowFilter so the ex-caller's audio is no longer captured.
//
// Requires the owner bot to have the Manage Roles permission in the test guild.
// The test restores the caller role in cleanup so it does not affect other tests.
func TestE12_AllowFilterUpdatedOnRoleRevoke(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need second speaker to verify filter")
	skipIfMissing(t, h.cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Both sources join before the raid.
	stopSource1, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker1.StartPlaying: %v", err)
	}
	stopSource2, err := h.Speaker2.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker2.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID, h.cfg.Speaker2ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeGuildCaller)
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
		stopSource1()
		stopSource2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Establish baseline — speaker1 relays cross-channel audio.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)

	// Fetch speaker2 source bot's current member to get its full role list.
	// Uses the listener bot (test-admin) so the owner bot needs no extra permissions.
	sourceID := h.Speaker2.ID()
	member, err := h.Listener.GetMember(h.cfg.GuildID, sourceID)
	if err != nil {
		t.Skipf("E12: GetMember failed (listener bot may lack permission): %v", err)
	}

	// Remove the caller role from speaker2 source. Restore in cleanup regardless.
	rolesWithout := filterIDs(member.RoleIDs, h.cfg.CallerRoleID)
	t.Cleanup(func() {
		_, _ = h.Listener.UpdateMember(h.cfg.GuildID, sourceID,
			discord.MemberUpdate{Roles: &member.RoleIDs})
	})
	if _, err := h.Listener.UpdateMember(h.cfg.GuildID, sourceID,
		discord.MemberUpdate{Roles: &rolesWithout}); err != nil {
		t.Skipf("E12: UpdateMember failed (listener bot may lack Manage Roles permission): %v", err)
	}
	t.Log("E12: caller role removed from speaker2 source, waiting for AllowFilter to reject it...")

	// onGuildMemberUpdate fires → NotifyMemberUpdate → AllowFilter.Set(sourceID, false).
	// Give the event a moment to propagate, then verify speaker2 source is no longer
	// producing relay frames (its audio is dropped by the VoiceReceiver).
	time.Sleep(2 * time.Second)
	baseAfterRevoke := h.Listener.Receiver.FramesReceived(speakerIDs[1])
	time.Sleep(2 * time.Second)
	deltaAfterRevoke := h.Listener.Receiver.FramesReceived(speakerIDs[1]) - baseAfterRevoke

	// With the role removed the cross-channel audio from speaker2 source should have
	// stopped — expect a very low frame delta (< 10 % of what 2 s of full audio gives).
	maxExpected := int64(2 * 50 / 10) // 10 frames
	if deltaAfterRevoke > maxExpected {
		t.Fatalf("E12 failed: after role revoke still got %d frames from speakerIDs[1] (expected < %d)", deltaAfterRevoke, maxExpected)
	}
	t.Logf("E12 passed: AllowFilter updated via onGuildMemberUpdate — %d frames after revoke (< %d)", deltaAfterRevoke, maxExpected)
}

// filterIDs returns a copy of ids with target removed.
func filterIDs(ids []snowflake.ID, target snowflake.ID) []snowflake.ID {
	out := make([]snowflake.ID, 0, len(ids))
	for _, id := range ids {
		if id != target {
			out = append(out, id)
		}
	}
	return out
}
