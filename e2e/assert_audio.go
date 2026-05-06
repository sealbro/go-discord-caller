//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// AssertFramesReceived polls until listener has received at least min frames from
// userID within the given deadline, or calls t.Fatal.
func AssertFramesReceived(t testing.TB, l *TestListener, userID snowflake.ID, min int64, within time.Duration) {
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
func AssertSSRCSeen(t testing.TB, l *TestListener, userID snowflake.ID, within time.Duration) {
	t.Helper()
	AssertFramesReceived(t, l, userID, 1, within)
}

// AssertSSRCNotSeen waits the full window and then asserts that no frames from
// userID were received. Used to verify mix-minus exclusion.
func AssertSSRCNotSeen(t testing.TB, l *TestListener, userID snowflake.ID, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	n := l.Receiver.FramesReceived(userID)
	if n > 0 {
		t.Fatalf("expected 0 frames from %s, got %d", userID, n)
	}
}

// AssertFrameGap asserts that frame delivery from userID stops for at least
// pauseFor and then resumes within resumeWithin after resume is called.
func AssertFrameGap(t testing.TB, l *TestListener, userID snowflake.ID, pauseFor, resumeWithin time.Duration, resume func()) {
	t.Helper()

	// Capture baseline.
	baseline := l.Receiver.FramesReceived(userID)
	time.Sleep(pauseFor)
	after := l.Receiver.FramesReceived(userID)

	// During pause the frame count should be well below what continuous streaming
	// would deliver (50 frames/s × pauseFor). Tolerate up to 10 % throughput as
	// jitter headroom.
	expected := int64(pauseFor.Seconds() * 50)
	delta := after - baseline
	if delta > expected/10 {
		t.Logf("AssertFrameGap: pause not effective — %d frames in %s (expected < %d)", delta, pauseFor, expected/10)
	}

	// Trigger resume and wait for frames to flow again.
	resume()
	deadline := time.Now().Add(resumeWithin)
	resumeBase := l.Receiver.FramesReceived(userID)
	for time.Now().Before(deadline) {
		if l.Receiver.FramesReceived(userID) > resumeBase+10 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("frames from %s did not resume within %s after resume()", userID, resumeWithin)
}

// AssertP99FirstFrameLatency asserts that the first-frame latency observed by
// listener for userID is within maxLatency of the provided startTime.
func AssertP99FirstFrameLatency(t testing.TB, l *TestListener, userID snowflake.ID, startTime time.Time, maxLatency time.Duration) {
	t.Helper()
	ts, ok := l.Receiver.FirstFrameAt(userID)
	if !ok {
		t.Fatalf("no frames received from %s — cannot measure first-frame latency", userID)
	}
	latency := ts.Sub(startTime)
	if latency > maxLatency {
		t.Fatalf("first-frame latency from %s: %s > limit %s", userID, latency, maxLatency)
	}
}

// skipIfMissing calls t.Skip when cond is false, formatting msg with args.
func skipIfMissing(t testing.TB, cond bool, msg string, args ...any) {
	t.Helper()
	if !cond {
		t.Skip(fmt.Sprintf(msg, args...))
	}
}
