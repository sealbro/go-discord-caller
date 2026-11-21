package opus

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A mixer that has consumed frames and then gone idle is auto-paused.
func TestDrainWatcher_PausesIdleMixer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestMixer(t)
		m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		const idle = 40 * time.Millisecond
		go NewDrainWatcher(m, idle).Run(t.Context())

		time.Sleep(idle / 2)
		synctest.Wait()
		if !m.Paused() {
			t.Fatal("idle mixer should be auto-paused at the watcher's first check")
		}
	})
}

// A relay-fed mixer must never be auto-paused: it is fed by another guild, and
// once paused it drains those frames without recording activity, so it could
// never unpause itself (issue #51).
func TestDrainWatcher_KeepAliveBlocksAutoPause(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestMixer(t)
		m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		const idle = 40 * time.Millisecond
		var live atomic.Bool
		live.Store(true)
		go NewDrainWatcher(m, idle).WithKeepAlive(live.Load).Run(t.Context())

		time.Sleep(10 * idle)
		synctest.Wait()
		if m.Paused() {
			t.Fatal("relay-fed mixer must not be auto-paused while its feed is live")
		}

		// Once the peer guild detaches the watcher resumes ownership.
		live.Store(false)
		time.Sleep(idle / 2)
		synctest.Wait()
		if !m.Paused() {
			t.Fatal("mixer should be auto-paused at the first check after the relay feed is gone")
		}
	})
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
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		src := newSource()
		m.AddInput(1, src)

		const idle = 200 * time.Millisecond
		go NewDrainWatcher(m, idle).Run(t.Context())

		frames := generateTestFrames(t, 440, 4, 8000)

		// Phase 1 — the mixer carries audio, so it must stay unpaused.
		stopPump := startPump(t.Context(), src, frames)
		runTicks(2 * int(idle/mixerFrameDur))
		if col.count() == 0 {
			t.Fatal("mixer should emit while audio flows")
		}
		if m.Paused() {
			t.Fatal("mixer must not be paused while audio is flowing")
		}

		// Phase 2 — the lull. The caller stays connected, it just stops talking.
		// The watcher checks every idle/2, so it pauses within idle + idle/2.
		stopPump()
		waitFor(t, idle+idle/2, m.Paused, "idle mixer should be auto-paused")

		// Phase 3 — the caller speaks again. The mixer must wake up at the
		// watcher's next check and resume emitting. With the latch present it
		// stays paused forever, because a paused mixer drains its inputs
		// without refreshing lastActivityAt, so IdleFor() only ever grows.
		before := col.count()
		t.Cleanup(startPump(t.Context(), src, frames))

		waitFor(t, idle/2+mixerFrameDur, func() bool { return !m.Paused() },
			"mixer must unpause once audio resumes; it latched paused and is discarding every frame")
		waitFor(t, 2*mixerFrameDur, func() bool { return col.count() > before },
			"mixer must emit again once audio resumes")
	})
}

// TestDrainWatcher_DoesNotOverrideRouterPause pins the pause-ownership rule.
//
// The router pauses destinations with no human listener. Now that a paused
// mixer records arriving audio as activity, IdleFor() stays small while its
// callers talk — so a watcher that simply mirrored "idle?" onto SetPaused
// would immediately undo the router and emit into an empty channel. It must
// only reverse pauses it applied itself.
func TestDrainWatcher_DoesNotOverrideRouterPause(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		src := newSource()
		m.AddInput(1, src)

		const idle = 200 * time.Millisecond
		go NewDrainWatcher(m, idle).Run(t.Context())

		// Audio flows continuously for the whole test.
		t.Cleanup(startPump(t.Context(), src, generateTestFrames(t, 440, 4, 8000)))
		runTicks(2)
		if m.Paused() {
			t.Fatal("mixer should be running while audio flows")
		}

		// The router pauses it — the destination lost its last listener.
		m.SetPaused(true)
		synctest.Wait()
		emitted := col.count()

		// Several watcher ticks pass with audio still arriving. The pause holds.
		time.Sleep(5 * idle)
		synctest.Wait()
		if !m.Paused() {
			t.Fatal("router pause was overridden by DrainWatcher; a destination with no listeners must stay paused")
		}
		if got := col.count() - emitted; got != 0 {
			t.Errorf("router-paused mixer emitted %d frames, want 0", got)
		}
	})
}
