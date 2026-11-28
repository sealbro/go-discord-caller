//go:build integration

package integration

import (
	"context"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// deafWait bounds how long a server-deaf change may take to show up over REST.
// Undeafening is immediate and deafening waits the controller's 5 s delay, so
// this leaves a few seconds for Discord on top of the slower of the two.
const deafWait = 10 * time.Second

// idleCPUBudget is the share of one core an idle raid may use. A healthy idle
// raid sits in the low single-digit percent; a busy-looping voice goroutine
// takes a whole core on its own.
const idleCPUBudget = 0.5

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

// requireListenerCaller gives the listener the caller role for the rest of the
// test, if it lacks it, and takes it back afterwards. The listener sits in the
// speaker's channel, so holding the role makes it that channel's caller.
func requireListenerCaller(t *testing.T) {
	t.Helper()
	guildID, roleID, listenerID := h.Cfg.GuildID, h.Cfg.CallerRoleID, h.Listener.ID()

	member, err := h.Listener.GetMember(guildID, listenerID)
	if err != nil {
		t.Fatalf("read listener member: %v", err)
	}
	if slices.Contains(member.RoleIDs, roleID) {
		return
	}
	requireRoleManagement(t)

	if err := h.Listener.AddMemberRole(guildID, listenerID, roleID); err != nil {
		t.Fatalf("grant the caller role to the listener: %v", err)
	}
	t.Cleanup(func() { _ = h.Listener.RemoveMemberRole(guildID, listenerID, roleID) })
}

// TestE20_CaptureRaidRepairsStrandedDeaf is the reconcile half of the
// server-deafen path (E19, in TestE1_OneCallerLifecycle, is the non-capture
// half).
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
//
// The speaker's channel needs a caller throughout. Without one the bot deafens
// the speaker itself and undoes that on stop, erasing the stranded flag.
func TestE20_CaptureRaidRepairsStrandedDeaf(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID
	requireListenerCaller(t)

	stopSource := startCaller(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)

	_, firstCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, firstCancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	// Only the bound speaker joins the raid.
	speakerID := h.RequireSpeakers(t)[0]
	h.RegisterCleanup(t, mgr, stopSource, stopListener)
	requireDeafenPower(t, speakerID)

	// Belt and braces: whatever this test does, do not leave the guild with a
	// deafened bot.
	t.Cleanup(func() {
		_ = h.Listener.SetServerDeaf(guildID, speakerID, false)
	})

	// The capture join must have left the speaker hearing before the strand,
	// or a late reconcile could erase it.
	waitForDeaf(t, guildID, speakerID, false, deafWait, "capture-mode speaker should hear")

	// Strand the flag while the speaker is connected (Discord rejects the PATCH
	// otherwise). Capture-mode teardown has no undeafen step, so this survives
	// the stop below — the same state a crash leaves behind.
	if err := h.Listener.SetServerDeaf(guildID, speakerID, true); err != nil {
		t.Fatalf("strand deaf flag on speaker %s: %v", speakerID, err)
	}
	waitForDeaf(t, guildID, speakerID, true, deafWait, "stranded deaf flag should apply")

	if err := mgr.StopVoiceRaid(ctx, guildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}

	// Confirm the strand survived teardown, so a pass below means the reconcile
	// ran rather than that there was nothing to repair.
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
	waitForDeaf(t, guildID, speakerID, false, deafWait,
		"capture raid did not repair the stranded deaf flag — capture is silent")
	t.Log("E20 passed: capture-mode join cleared the stranded deaf flag")
}

// TestE21_IdleRaidAndDeafFollowsCallerPresence runs a capture raid that starts
// with nobody to capture and checks two things on it.
//
// The dynamic deafen path end-to-end: a capture bot whose channel holds nobody
// worth capturing gets deafened, and hearing is restored the moment a caller
// arrives. The test drives that direction: start with an empty channel
// (case 2), then have a caller arrive (case 6). The leave direction is covered
// by TestDeafControllerFollowsCallerPresence in internal/manager.
//
// The idle CPU cost, measured before the caller arrives. Production,
// 2026-10-25: a raid pinned the host's only core for hours, and every bot's
// gateway heartbeat slowed from ~110 ms to ~545 ms behind it. The spin was
// silent — no allocations, no logs — so no assertion on audio or state can see
// it; only CPU time does. Every bot in this harness runs in the test process,
// so its CPU time covers all of them.
func TestE21_IdleRaidAndDeafFollowsCallerPresence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	// No caller in the owner channel: the owner bot captures nothing.
	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	raidStarted := time.Now()
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)

	speakerID := h.RequireSpeakers(t)[0]
	h.RegisterCleanup(t, mgr, stopListener)
	t.Cleanup(func() { _ = h.Listener.SetServerDeaf(guildID, h.OwnerID, false) })

	phase(t, "E21 owner deafened with an empty channel", func(t *testing.T) {
		requireDeafenPower(t, speakerID)
		waitForDeaf(t, guildID, h.OwnerID, true, deafWait,
			"owner should be deafened while its channel has no callers")
	})

	phase(t, "idle raid does not spin CPU", func(t *testing.T) {
		// Let joins and DAVE handshakes finish, so the window measures the raid
		// at rest rather than its start-up.
		const settle = 10 * time.Second
		time.Sleep(settle - time.Since(raidStarted))

		const window = 10 * time.Second
		before := processCPUTime(t)
		time.Sleep(window)
		used := processCPUTime(t) - before

		share := used.Seconds() / window.Seconds()
		if share > idleCPUBudget {
			t.Fatalf("idle raid used %v of CPU in %v (%.0f%% of one core, budget %.0f%%): "+
				"a voice goroutine is busy-looping", used.Round(time.Millisecond), window, share*100, idleCPUBudget*100)
		}
		t.Logf("idle raid used %v of CPU in %v (%.1f%% of one core)", used.Round(time.Millisecond), window, share*100)
	})

	phase(t, "E21 owner undeafened when a caller joins", func(t *testing.T) {
		requireDeafenPower(t, speakerID)
		// Undeafening must not wait out the deafen delay, so the window here
		// is deliberately tight.
		joinedAt := time.Now()
		stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
		t.Cleanup(stopSource)
		waitForDeaf(t, guildID, h.OwnerID, false, deafWait,
			"owner should hear as soon as a caller joins")
		t.Logf("E21: undeafened %v after the caller joined", time.Since(joinedAt).Round(time.Second))
	})
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
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: deaf = %v after %v, want %v", msg, last, timeout, want)
}

// processCPUTime is the user plus system CPU time this process has used.
func processCPUTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
