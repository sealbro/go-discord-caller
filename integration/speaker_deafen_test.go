//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// deafSettleDelay gives Discord time to apply a member voice-state PATCH and
// propagate it before the test reads the member back over REST.
const deafSettleDelay = 2 * time.Second

// requireDeafenPower skips the test when the owner bot cannot server-deafen the
// given speaker in this guild — missing DEAFEN_MEMBERS, or a role that does not
// outrank the speaker's. Both are guild configuration, not code defects, and
// the manager deliberately degrades to a warning in that case, so failing here
// would only report the environment.
func requireDeafenPower(t *testing.T, speakerID snowflake.ID) {
	t.Helper()
	ok, why, err := h.Listener.CanDeafen(h.Cfg.GuildID, h.OwnerID, speakerID)
	if err != nil {
		t.Fatalf("check deafen power: %v", err)
	}
	if !ok {
		t.Skipf("server-deafen not available in this guild: %s "+
			"(grant Deafen Members to the owner bot and place its role above the speakers)", why)
	}
}

// TestE19_NonCaptureSpeakersAreServerDeafened covers the whole server-deafen
// path in a non-capture mode (RaidModeOneCaller): the speaker is deafened for
// the life of the raid, keeps relaying audio while deafened, and is undeafened
// on teardown.
//
// The middle assertion is the load-bearing one. Server deafen is the only lever
// that stops Discord forwarding RTP — self_deaf is ignored by the voice server
// (measured 2026-09-09; see internal/manager/speaker_deafen.go) — and the whole
// optimisation rests on it being receive-only. If Discord ever made server
// deafen suppress outbound too, every non-capture raid would go silent. This is
// the test that catches that.
func TestE19_NonCaptureSpeakersAreServerDeafened(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	// Child cancel so StopVoiceRaid does not cancel the test's own ctx.
	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	// Only speakers bound to a channel join the raid, and this raid binds one —
	// RequireSpeakers reports the whole connected pool, so assert on the joiner.
	speakerIDs := h.RequireSpeakers(t)
	speakerID := speakerIDs[0]
	h.RegisterCleanup(t, mgr, stopSource, stopListener)
	requireDeafenPower(t, speakerID)

	time.Sleep(deafSettleDelay)

	deaf, err := h.Listener.MemberServerDeaf(guildID, speakerID)
	if err != nil {
		t.Fatalf("read speaker %s member: %v", speakerID, err)
	}
	if !deaf {
		t.Fatalf("speaker %s is not server-deafened during a non-capture raid", speakerID)
	}
	t.Log("E19: speaker server-deafened; checking audio still flows")

	// Deafened speakers must still relay. This is the receive-only property.
	AssertFramesReceived(t, h.Listener, speakerID, 100, 20*time.Second)
	t.Logf("E19: %d frames relayed while deafened", h.Listener.Receiver.FramesReceived(speakerID))

	if err := mgr.StopVoiceRaid(ctx, guildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	time.Sleep(deafSettleDelay)

	deaf, err = h.Listener.MemberServerDeaf(guildID, speakerID)
	if err != nil {
		t.Fatalf("read speaker %s member after stop: %v", speakerID, err)
	}
	if deaf {
		t.Fatalf("speaker %s left server-deafened after raid teardown", speakerID)
	}
	t.Log("E19 passed: deafened during raid, relaying throughout, undeafened after")
}

// TestE20_CaptureRaidRepairsStrandedDeaf is the reconcile half.
//
// A server-deaf flag persists on the guild member across voice sessions, and a
// deafened bot receives no RTP at all — so a flag stranded by a crash mid
// non-capture raid would make the *next* capture raid join that speaker, look
// healthy, and capture nothing. reconcileSpeakerDeaf clears the flag on every
// capture-mode join for exactly this reason.
//
// The test strands the flag the way a crash does: deafen the speaker while it
// is connected during a capture raid (capture teardown has no undeafen step, so
// the flag survives the stop), then start a fresh capture raid and require that
// the join repaired it.
func TestE20_CaptureRaidRepairsStrandedDeaf(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	_, firstCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, firstCancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	// Only the bound speaker joins; see E17.
	speakerIDs := h.RequireSpeakers(t)
	speakerID := speakerIDs[0]
	h.RegisterCleanup(t, mgr, stopSource, stopListener)
	requireDeafenPower(t, speakerID)

	// Belt and braces: whatever this test does, do not leave the guild with a
	// deafened bot.
	t.Cleanup(func() {
		_ = h.Listener.SetServerDeaf(guildID, speakerID, false)
	})

	time.Sleep(deafSettleDelay)

	// Strand the flag while the speaker is connected (Discord rejects the PATCH
	// otherwise). Capture-mode teardown has no undeafen step, so this survives
	// the stop below — the same state a crash leaves behind.
	if err := h.Listener.SetServerDeaf(guildID, speakerID, true); err != nil {
		t.Fatalf("strand deaf flag on speaker %s: %v", speakerID, err)
	}
	time.Sleep(deafSettleDelay)

	if err := mgr.StopVoiceRaid(ctx, guildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	time.Sleep(deafSettleDelay)

	// Confirm the strand actually took, so a pass below means the reconcile ran
	// rather than that there was nothing to repair.
	deaf, err := h.Listener.MemberServerDeaf(guildID, speakerID)
	if err != nil {
		t.Fatalf("read speaker %s member: %v", speakerID, err)
	}
	if !deaf {
		t.Fatalf("speaker %s was not left deafened — the test cannot prove the repair", speakerID)
	}
	t.Log("E20: deaf flag stranded across teardown; restarting capture raid")

	_, secondCancel := context.WithCancel(ctx)
	if _, err := mgr.StartVoiceRaid(ctx, guildID, secondCancel, guild.RaidModeGuildCaller); err != nil {
		t.Fatalf("StartVoiceRaid (restart): %v", err)
	}
	time.Sleep(deafSettleDelay)

	deaf, err = h.Listener.MemberServerDeaf(guildID, speakerID)
	if err != nil {
		t.Fatalf("read speaker %s member after restart: %v", speakerID, err)
	}
	if deaf {
		t.Fatalf("capture raid did not repair stranded deaf flag on speaker %s — capture is silent", speakerID)
	}
	t.Log("E20 passed: capture-mode join cleared the stranded deaf flag")
}

// TestE21_DeafFollowsCallerPresence is the dynamic path end-to-end: a capture
// bot whose channel holds nobody worth capturing gets deafened, and hearing is
// restored the moment a caller arrives.
//
// Direction matters here for a harness reason. onVoiceLeave (internal/bot/
// handlers.go) returns early for bots before reaching AutoRoute, and every
// "caller" in the test guild is a bot standing in for a human — so a caller
// *leaving* never triggers a recompute in this environment, though it does in
// production where callers are people. Joins and session start both recompute
// for bots, so the test drives those: start with an empty channel (case 2),
// then have a caller arrive (case 6). The leave direction is covered by
// TestCaptureObserverFollowsCallerPresence at the router level.
func TestE21_DeafFollowsCallerPresence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	// No caller in the owner channel: the owner bot captures nothing.
	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopListener)
	requireDeafenPower(t, speakerIDs[0])

	t.Cleanup(func() { _ = h.Listener.SetServerDeaf(guildID, h.OwnerID, false) })

	// Case 2: capture bot in a channel with nobody holding the role.
	waitForDeaf(t, guildID, h.OwnerID, true, 60*time.Second,
		"owner should be deafened while its channel has no callers")
	t.Log("E21: owner deafened with an empty channel")

	// Case 6: a role-bearing member joins. Undeafening must not wait out the
	// deafen delay, so the window here is deliberately tight.
	joinedAt := time.Now()
	stopSource, err := h.Speaker.StartPlaying(ctx, guildID, h.Cfg.OwnerChannelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("caller join: %v", err)
	}
	t.Cleanup(stopSource)

	waitForDeaf(t, guildID, h.OwnerID, false, 20*time.Second,
		"owner should hear as soon as a caller joins")
	t.Logf("E21 passed: undeafened %v after the caller joined", time.Since(joinedAt).Round(time.Second))
}

// waitForDeaf polls a member's server-deaf flag until it reaches want.
func waitForDeaf(t *testing.T, guildID, userID snowflake.ID, want bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last bool
	for time.Now().Before(deadline) {
		got, err := h.Listener.MemberServerDeaf(guildID, userID)
		if err != nil {
			t.Fatalf("read member %s: %v", userID, err)
		}
		last = got
		if got == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: deaf = %v after %v, want %v", msg, last, timeout, want)
}
