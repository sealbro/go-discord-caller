//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE10_CallerJoinAfterRaidStart verifies the onVoiceJoin live-join path:
// a caller who joins the owner channel AFTER the raid is already active is picked
// up by the handler → NotifyMemberUpdate → AllowFilter updated → their audio is
// captured and relayed. This is distinct from E1 (caller present at
// StartVoiceRaid, captured via the RequestMembers prefetch).
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

// TestE12_RoleChangesFollowedMidRaid verifies the onGuildMemberUpdate path:
// taking the caller role from the only caller mid-session must stop its audio
// reaching the speaker channel (NotifyMemberUpdate updates the AllowFilter and
// re-runs the router), and giving it back must bring the audio back.
//
// Requires the listener bot to be able to manage the caller role: MANAGE_ROLES
// and a highest role above it. The role is restored in cleanup so it does not
// affect other tests.
func TestE12_RoleChangesFollowedMidRaid(t *testing.T) {
	requireRoleManagement(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	callerID := h.Speaker.ID()
	stopSource := startCaller(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerID := h.RequireSpeakers(t)[0]
	h.RegisterCleanup(t, mgr, stopSource, stopListener)

	phase(t, "E12 caller relayed", func(t *testing.T) {
		AssertFramesReceived(t, h.Listener, speakerID, 50, 8*time.Second)
	})

	scenario := t
	phase(t, "E12 role revoke stops capture", func(t *testing.T) {
		revokeCallerRole(scenario, callerID)
		AssertFramesStop(t, h.Listener, speakerID, 5*time.Second)
	})

	phase(t, "E12 role grant resumes capture", func(t *testing.T) {
		if err := h.Listener.AddMemberRole(h.Cfg.GuildID, callerID, h.Cfg.CallerRoleID); err != nil {
			t.Fatalf("give the caller role back: %v", err)
		}
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 10*time.Second)
	})
}
