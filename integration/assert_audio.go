//go:build integration || stress

package integration

import (
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/integration/test"
)

// AssertFramesReceived polls until listener has received at least min frames from
// userID within the given deadline, or calls t.Fatal.
func AssertFramesReceived(t testing.TB, l *test.Listener, userID snowflake.ID, min int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if l.Receiver.FramesReceived(userID) >= min {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected >= %d frames from %s within %s, got %d",
		min, userID, within, l.Receiver.FramesReceived(userID))
}

// AssertSSRCSeen polls until at least one frame from userID is received within
// the given deadline, or calls t.Fatal.
func AssertSSRCSeen(t testing.TB, l *test.Listener, userID snowflake.ID, within time.Duration) {
	t.Helper()
	AssertFramesReceived(t, l, userID, 1, within)
}

// AssertFramesStop waits until frames from userID stop arriving — a full
// second with at most 10 % of the nominal 50 frames, leaving room for frames
// already in flight — or calls t.Fatal once within has passed.
func AssertFramesStop(t testing.TB, l *test.Listener, userID snowflake.ID, within time.Duration) {
	t.Helper()
	const window, maxInWindow = time.Second, 5
	deadline := time.Now().Add(within)
	for {
		base := l.Receiver.FramesReceived(userID)
		time.Sleep(window)
		delta := l.Receiver.FramesReceived(userID) - base
		if delta <= maxInWindow {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames from %s still arriving after %v: %d in the last %v (want <= %d)",
				userID, within, delta, window, maxInWindow)
		}
	}
}

// AssertFramesIncreasedBy captures the current frame count as a baseline and
// polls until delta additional frames arrive within the deadline, or calls t.Fatal.
// Use this after an interruption (reconnect, restart) to verify delivery resumed.
func AssertFramesIncreasedBy(t testing.TB, l *test.Listener, userID snowflake.ID, delta int64, within time.Duration) {
	t.Helper()
	base := l.Receiver.FramesReceived(userID)
	AssertFramesReceived(t, l, userID, base+delta, within)
}

// skipIfMissing calls t.Skip when cond is false, formatting msg with args.
func skipIfMissing(t testing.TB, cond bool, msg string, args ...any) {
	t.Helper()
	if !cond {
		t.Skipf(msg, args...)
	}
}
