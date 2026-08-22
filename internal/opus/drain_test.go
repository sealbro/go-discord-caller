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
