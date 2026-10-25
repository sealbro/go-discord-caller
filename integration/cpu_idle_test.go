//go:build integration

package integration

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// idleCPUBudget is the share of one core an idle raid may use. A healthy idle
// raid sits in the low single-digit percent; a busy-looping voice goroutine
// takes a whole core on its own.
const idleCPUBudget = 0.5

// Production, 2026-10-25: a raid pinned the host's only core for hours, and
// every bot's gateway heartbeat slowed from ~110 ms to ~545 ms behind it. The
// spin was silent — no allocations, no logs — so no assertion on audio or
// state can see it; only CPU time does. Every bot in this harness runs in the
// test process, so its CPU time covers all of them.
func TestIdleRaidDoesNotSpinCPU(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID

	_, sessionCancel := context.WithCancel(ctx)
	mgr := h.MustStartRaid(t, ctx, sessionCancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, guildID, h.Cfg.Speaker1ChannelID)
	h.RegisterCleanup(t, mgr, stopListener)

	// Let joins and DAVE handshakes finish, so the window measures the raid at
	// rest rather than its start-up.
	time.Sleep(10 * time.Second)

	const window = 10 * time.Second
	before := processCPUTime(t)
	time.Sleep(window)
	used := processCPUTime(t) - before

	if share := used.Seconds() / window.Seconds(); share > idleCPUBudget {
		t.Fatalf("idle raid used %v of CPU in %v (%.0f%% of one core, budget %.0f%%): "+
			"a voice goroutine is busy-looping", used.Round(time.Millisecond), window, share*100, idleCPUBudget*100)
	} else {
		t.Logf("idle raid used %v of CPU in %v (%.1f%% of one core)", used.Round(time.Millisecond), window, share*100)
	}
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
