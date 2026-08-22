package opus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A mixer that has consumed frames and then gone idle is auto-paused.
func TestDrainWatcher_PausesIdleMixer(t *testing.T) {
	t.Parallel()
	m, _ := newTestMixer(t)
	m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go NewDrainWatcher(m, 40*time.Millisecond).Run(ctx)

	waitFor(t, 2*time.Second, m.Paused, "idle mixer should be auto-paused")
}

// A relay-fed mixer must never be auto-paused: it is fed by another guild, and
// once paused it drains those frames without recording activity, so it could
// never unpause itself (issue #51).
func TestDrainWatcher_KeepAliveBlocksAutoPause(t *testing.T) {
	t.Parallel()
	m, _ := newTestMixer(t)
	m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var live atomic.Bool
	live.Store(true)
	go NewDrainWatcher(m, 40*time.Millisecond).WithKeepAlive(live.Load).Run(ctx)

	time.Sleep(300 * time.Millisecond)
	if m.Paused() {
		t.Fatal("relay-fed mixer must not be auto-paused while its feed is live")
	}

	// Once the peer guild detaches the watcher resumes ownership.
	live.Store(false)
	waitFor(t, 2*time.Second, m.Paused, "mixer should be auto-paused once the relay feed is gone")
}

// TestDrainWatcher_ResumesAfterLull is the unit-level reproduction of the
// production symptom "the speakers stopped hearing the caller a few seconds
// after the raid started".
//
// A mixer that has carried audio and then sits idle past the threshold is
// auto-paused, which is intended. What must also hold is that audio arriving
// afterwards wakes it up again: the callers never left the channel, so no
// voice-state event reaches the router and nothing else will ever call
// SetPaused to repair it.
//
// This is the same latch that WithKeepAlive works around for relay-fed mixers
// (see TestDrainWatcher_KeepAliveBlocksAutoPause), but an ordinary destination
// mixer has no such guard.
func TestDrainWatcher_ResumesAfterLull(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)

	src := newSource()
	if err := m.AddInput(1, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 200 * time.Millisecond
	go NewDrainWatcher(m, idle).Run(ctx)

	frames := generateTestFrames(t, 440, 4, 8000)

	// Phase 1 — the mixer carries audio, so it must stay unpaused.
	stopPump := startPump(ctx, src, frames)
	waitFor(t, 2*time.Second, func() bool { return col.count() > 0 }, "mixer should emit while audio flows")
	if m.Paused() {
		t.Fatal("mixer must not be paused while audio is flowing")
	}

	// Phase 2 — the lull. The caller stays connected, it just stops talking.
	stopPump()
	waitFor(t, 2*time.Second, m.Paused, "idle mixer should be auto-paused")

	// Phase 3 — the caller speaks again. The mixer must wake up and resume
	// emitting. With the latch present it stays paused forever, because a
	// paused mixer drains its inputs without refreshing lastActivityAt, so
	// IdleFor() only ever grows.
	before := col.count()
	stopPump2 := startPump(ctx, src, frames)
	defer stopPump2()

	waitFor(t, 3*time.Second, func() bool { return !m.Paused() },
		"mixer must unpause once audio resumes; it latched paused and is discarding every frame")
	waitFor(t, 3*time.Second, func() bool { return col.count() > before },
		"mixer must emit again once audio resumes")
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
