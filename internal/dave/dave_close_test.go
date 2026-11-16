package dave

import (
	"testing"

	"github.com/disgoorg/godave"
)

// disgo's conn Close closes the DAVE session without a RemoveUser per member,
// so the session itself must release every sender it still holds.
func TestClosingSessionReleasesItsSenders(t *testing.T) {
	stats := NewStats()
	session, backend := knownSession(t, stats, "bot-1", nil, "user-a", "user-b")

	for _, u := range []godave.UserID{"user-a", "user-b"} {
		if _, err := session.Decrypt(u, nil, make([]byte, 64)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	reader, _ := newReader(t, stats)
	_, _ = collectOnce(t, reader)
	pts, sp := collectOnce(t, reader)
	if got := findHealth(t, sp, healthClean); got != -1 {
		t.Errorf("%d senders of a closed session still in the gauge", got)
	}
	if got := find(t, pts, outcomeSuccess); got != 2 {
		t.Errorf("total = %d, want the 2 frames kept after release", got)
	}

	stats.mu.RLock()
	live, holders := len(stats.counters), len(stats.holders)
	stats.mu.RUnlock()
	if live != 0 || holders != 0 {
		t.Errorf("closed session left %d counters and %d holders in memory, want 0", live, holders)
	}
	if backend.closes != 1 {
		t.Errorf("backend closed %d times, want once", backend.closes)
	}
}

// A frame trailing a RemoveUser must not create a counter nothing retires.
func TestLateFrameAfterRemoveUserLeavesNoSenderSeries(t *testing.T) {
	stats := NewStats()
	session, _ := knownSession(t, stats, "bot-1", nil, "user-a")
	session.RemoveUser("user-a")

	reader, _ := newReader(t, stats)
	_, _ = collectOnce(t, reader)

	if _, err := session.Decrypt("user-a", []byte("late"), make([]byte, 0, 512)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pts, sp := collectOnce(t, reader)
	if got := find(t, pts, outcomePassthrough); got != 1 {
		t.Errorf("passthrough total = %d, want the late frame still counted once", got)
	}
	if len(sp) != 0 {
		t.Errorf("a departed sender is counted in the gauge: %v", sp)
	}

	stats.mu.RLock()
	defer stats.mu.RUnlock()
	for k := range stats.counters {
		if k.userID != UnknownUserID {
			t.Errorf("late frame created a per-sender series that is never retired")
		}
	}
}

func TestPassthroughRefusesFrameLargerThanBuffer(t *testing.T) {
	stats := NewStats()
	session, _ := newTestSession(t, stats, "bot-1", nil)

	frame := make([]byte, 600)
	n, err := session.Decrypt("user-a", frame, make([]byte, 0, 512))
	if err == nil {
		t.Fatalf("n = %d, nil error: a %d-byte frame was silently truncated to 512", n, len(frame))
	}
	if got := find(t, collect(t, stats), outcomeFailure); got != 1 {
		t.Errorf("failure count = %d, want the oversized frame counted once", got)
	}
}
