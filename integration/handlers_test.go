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

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	if _, ok := h.Pool.GetClientByID(speakerIDs[0]); !ok {
		t.Skip("speaker not connected")
	}
	// OP4 with channel_id=null → VOICE_STATE_UPDATE on the owner bot →
	// onVoiceLeave → ReconnectBotChannel.
	leaveCtx, leaveCancel := context.WithTimeout(ctx, 5*time.Second)
	h.DisconnectSpeakerVoice(leaveCtx, h.Cfg.GuildID, speakerIDs[0])
	leaveCancel()
	t.Log("E8: speaker voice connection dropped, waiting for ReconnectBotChannel...")

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 20*time.Second)
	t.Log("E8 passed: speaker reconnected and frames resumed after voice disconnect")
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

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Displace the speaker from its bound channel → GuildVoiceMove on the
	// owner bot → OnBotVoiceMove → ReconnectBotChannel.
	moveCtx, moveCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := h.MoveSpeakerVoice(moveCtx, h.Cfg.GuildID, speakerIDs[0], h.Cfg.OwnerChannelID); err != nil {
		moveCancel()
		t.Skipf("E9: move member failed (owner bot may lack Move Members permission): %v", err)
	}
	moveCancel()
	t.Log("E9: speaker moved to owner channel, waiting for ReconnectBotChannel...")

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 50, 20*time.Second)
	t.Log("E9 passed: speaker reconnected and frames resumed after voice move")
}

// TestE10_CallerJoinAfterRaidStart verifies the onVoiceJoin live-join path:
// a caller who joins the owner channel AFTER the raid is already active is picked
// up by the handler → NotifyMemberUpdate → AllowFilter updated → their audio is
// captured and relayed. This is distinct from E1 (caller present at StartVoiceRaid,
// captured via RequestMembers prefetch) and E7 (caller pre-joined, RequestMembers).
func TestE10_CallerJoinAfterRaidStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Empty owner channel at raid start so RequestMembers prefetch is a no-op
	// and the live onVoiceJoin path is the only way the caller can register.
	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)

	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Log("E10 passed: caller joined after raid start → captured via onVoiceJoin handler")
}

// TestE11_MixerPauseResumeViaVoiceEvents verifies the auto-pause/resume path
// driven by Discord voice events:
//   - onVoiceLeave (non-bot) → AutoRoute → router pauses the destination mixer
//     once HasListeners returns false (last human left the channel)
//   - onVoiceJoin  (non-bot) → AutoRoute → router unpauses when the caller rejoins
func TestE11_MixerPauseResumeViaVoiceEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)

	// stopSource is reassigned below; cleanup closure must capture by reference.
	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)
	t.Log("E11: baseline established, dropping caller...")

	stopSource()
	stopSource = func() {}

	AssertFrameGap(t, h.Listener, speakerIDs[0], 2*time.Second, 10*time.Second, func() {
		var err error
		stopSource, err = h.Speaker.StartPlaying(ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID, h.Cfg.SamplesDir)
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
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListener)

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 8*time.Second)

	// Use the listener (test-admin) for role mutation so the owner bot needs
	// no Manage Roles permission.
	sourceID := h.Speaker2.ID()
	member, err := h.Listener.GetMember(h.Cfg.GuildID, sourceID)
	if err != nil {
		t.Skipf("E12: GetMember failed (listener bot may lack permission): %v", err)
	}

	rolesWithout := filterIDs(member.RoleIDs, h.Cfg.CallerRoleID)
	t.Cleanup(func() {
		_, _ = h.Listener.UpdateMember(h.Cfg.GuildID, sourceID,
			discord.MemberUpdate{Roles: &member.RoleIDs})
	})
	if _, err := h.Listener.UpdateMember(h.Cfg.GuildID, sourceID,
		discord.MemberUpdate{Roles: &rolesWithout}); err != nil {
		t.Skipf("E12: UpdateMember failed (listener bot may lack Manage Roles permission): %v", err)
	}
	t.Log("E12: caller role removed from speaker2 source, waiting for AllowFilter to reject it...")

	// Give onGuildMemberUpdate a moment to propagate before sampling the post-revoke window.
	time.Sleep(2 * time.Second)
	baseAfterRevoke := h.Listener.Receiver.FramesReceived(speakerIDs[1])
	time.Sleep(2 * time.Second)
	deltaAfterRevoke := h.Listener.Receiver.FramesReceived(speakerIDs[1]) - baseAfterRevoke

	// 2 s of full audio = 100 frames; allow ≤ 10 % for in-flight before the filter took effect.
	maxExpected := int64(2 * 50 / 10)
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
