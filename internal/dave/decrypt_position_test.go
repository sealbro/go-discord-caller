package dave

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/disgoorg/godave"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// clockedSession returns a session that already knows user-a, reading time
// from clock.
func clockedSession(t *testing.T, stats *Stats, clock *fakeClock) (godave.Session, *fakeSession) {
	t.Helper()
	backend := &fakeSession{known: make(map[godave.UserID]struct{})}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	session := Instrument(inner, stats, withClock(clock.now))(slog.Default(), "bot-1", nil)
	session.AddUser("user-a")
	return session, backend
}

// speak delivers n frames from user-a, one per 20 ms.
func speak(session godave.Session, clock *fakeClock, n int) {
	for range n {
		_, _ = session.Decrypt("user-a", nil, make([]byte, 64))
		clock.advance(20 * time.Millisecond)
	}
}

func positionPoints(t *testing.T, reader *metric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.dave.decrypt.position.total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gdc.dave.decrypt.position.total is %T, want Sum[int64]", md.Data)
			}
			return sum.DataPoints
		}
	}
	return nil
}

func positionCount(points []metricdata.DataPoint[int64], outcome, position string) int64 {
	for _, p := range points {
		o, _ := p.Attributes.Value("outcome")
		pos, _ := p.Attributes.Value("position")
		if o.AsString() == outcome && pos.AsString() == position {
			return p.Value
		}
	}
	return 0
}

// The question the counter exists to answer: do decrypt failures cluster at
// the start of a talk spurt, or spread through speech? Frames are bucketed by
// their index since the sender last went quiet.
func TestDecryptFramesAreBucketedByPositionInTheTalkSpurt(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, _ := clockedSession(t, stats, clock)

	speak(session, clock, 12) // first, 9 early, 2 mid

	reader, _ := newReader(t, stats)
	pts := positionPoints(t, reader)
	for _, tc := range []struct {
		position string
		want     int64
	}{{positionFirst, 1}, {positionEarly, 9}, {positionMid, 2}} {
		if got := positionCount(pts, outcomeSuccess, tc.position); got != tc.want {
			t.Errorf("successful %s frames = %d, want %d", tc.position, got, tc.want)
		}
	}
}

func TestSilenceStartsANewTalkSpurt(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, _ := clockedSession(t, stats, clock)

	speak(session, clock, 3)
	clock.advance(talkSpurtGap)
	speak(session, clock, 1)

	reader, _ := newReader(t, stats)
	if got := positionCount(positionPoints(t, reader), outcomeSuccess, positionFirst); got != 2 {
		t.Errorf("first frames = %d, want 2: a pause of %v starts a new spurt", got, talkSpurtGap)
	}
}

// A lost packet or two is jitter, not the sender going quiet.
func TestShortGapDoesNotStartANewTalkSpurt(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, _ := clockedSession(t, stats, clock)

	speak(session, clock, 3)
	clock.advance(40 * time.Millisecond)
	speak(session, clock, 1)

	reader, _ := newReader(t, stats)
	if got := positionCount(positionPoints(t, reader), outcomeSuccess, positionFirst); got != 1 {
		t.Errorf("first frames = %d, want 1: a 60 ms gap is jitter", got)
	}
}

func TestDecryptFailuresAreCountedAtTheirPosition(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, backend := clockedSession(t, stats, clock)

	backend.err = errors.New("failed to decrypt frame")
	speak(session, clock, 2) // first + 1 early fail
	backend.err = nil
	speak(session, clock, 10) // 8 early + 2 mid succeed

	reader, _ := newReader(t, stats)
	pts := positionPoints(t, reader)
	if got := positionCount(pts, outcomeFailure, positionFirst); got != 1 {
		t.Errorf("failed first frames = %d, want 1", got)
	}
	if got := positionCount(pts, outcomeFailure, positionEarly); got != 1 {
		t.Errorf("failed early frames = %d, want 1", got)
	}
	if got := positionCount(pts, outcomeSuccess, positionEarly); got != 8 {
		t.Errorf("successful early frames = %d, want 8", got)
	}
	if got := positionCount(pts, outcomeFailure, positionMid); got != 0 {
		t.Errorf("failed mid frames = %d, want 0", got)
	}
}

// Frames the wrapper handles itself (no decryptor for the sender) never reach
// the backend, so they say nothing about decrypt timing.
func TestUnknownSenderFramesHaveNoPosition(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, _ := clockedSession(t, stats, clock)

	_, _ = session.Decrypt(godave.UserID(UnknownUserID), nil, make([]byte, 64))

	reader, _ := newReader(t, stats)
	if pts := positionPoints(t, reader); len(pts) != 0 {
		t.Errorf("position points for an unknown sender = %d, want 0", len(pts))
	}
}

func TestDecryptPositionExportsNoSenderIdentity(t *testing.T) {
	stats := NewStats()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	session, _ := clockedSession(t, stats, clock)
	speak(session, clock, 1)

	reader, _ := newReader(t, stats)
	for _, p := range positionPoints(t, reader) {
		for _, kv := range p.Attributes.ToSlice() {
			if kv.Value.AsString() == "user-a" {
				t.Errorf("a sender identity leaked through label %q", kv.Key)
			}
		}
	}
}
