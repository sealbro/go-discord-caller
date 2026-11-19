package dave

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/godave"
)

// stallingSession is a fakeSession whose encryptor has no key ratchet.
type stallingSession struct {
	*fakeSession
}

func (s *stallingSession) Encrypt(uint32, []byte, []byte) (int, error) {
	return 0, errMissingKeyRatchet
}

// steppingClock advances by one second on every reading.
func steppingClock() func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func traceSession(t *testing.T, rec *stallRecorder, stallFrames int) godave.Session {
	t.Helper()
	backend := &stallingSession{&fakeSession{known: make(map[godave.UserID]struct{})}}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	return Instrument(inner, NewStats(), withClock(steppingClock()), WithEncryptStall(stallFrames, rec.notify))(
		slog.Default(), "speaker-1", &fakeConn{})
}

// The cause of a stalled encryptor is the order in which the voice gateway's
// DAVE messages arrived, and the logs hold nothing about them. A stall report
// carries the signalling that led up to it.
func TestStallReportCarriesTheDaveSignalling(t *testing.T) {
	rec := &stallRecorder{}
	session := traceSession(t, rec, 3)

	session.OnSelectProtocolAck(1)
	session.OnDavePrepareEpoch(1, 1)
	session.AddUser("user-a")
	session.OnDavePrepareTransition(5, 1)
	session.OnDaveExecuteTransition(5)
	session.OnDaveMLSWelcome(6, nil)
	session.OnDaveMLSPrepareCommitTransition(7, nil)
	session.RemoveUser("user-a")
	encryptN(session, 3)

	if len(rec.traces) != 1 {
		t.Fatalf("stall reports = %d, want 1", len(rec.traces))
	}
	var got []string
	for _, e := range rec.traces[0] {
		got = append(got, e.String())
	}
	want := []string{
		"ack v1 ready=false others=0",
		"prepare_epoch e1 v1 ready=false others=0",
		"add_user ready=false others=1",
		"prepare_transition t5 v1 ready=false others=1",
		"execute_transition t5 ready=false others=1",
		"welcome t6 ready=false others=1",
		"commit t7 ready=false others=1",
		"remove_user ready=false others=0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := 1; i < len(rec.traces[0]); i++ {
		if rec.traces[0][i].At <= rec.traces[0][i-1].At {
			t.Errorf("event %d at %v, not after event %d at %v", i, rec.traces[0][i].At, i-1, rec.traces[0][i-1].At)
		}
	}
}

// A user ID is a persistent identifier for a real account, and the trace goes
// into the logs: it may count the channel, never name anyone in it.
func TestTraceLogValueNamesNoUser(t *testing.T) {
	rec := &stallRecorder{}
	session := traceSession(t, rec, 1)

	session.AddUser("user-a")
	session.RemoveUser("user-a")
	encryptN(session, 1)

	if len(rec.traces) != 1 {
		t.Fatalf("stall reports = %d, want 1", len(rec.traces))
	}
	rendered := rec.traces[0].LogValue().String()
	if strings.Contains(rendered, "user-a") {
		t.Errorf("trace log value names a user: %s", rendered)
	}
	if !strings.Contains(rendered, "add_user") {
		t.Errorf("trace log value lost its events: %s", rendered)
	}
}

// A session lives as long as its voice connection; the trace must stay small.
func TestTraceKeepsOnlyTheMostRecentEvents(t *testing.T) {
	rec := &stallRecorder{}
	session := traceSession(t, rec, 1)

	for i := range traceLen + 8 {
		session.OnDavePrepareTransition(uint16(i+1), 1)
	}
	encryptN(session, 1)

	if len(rec.traces) != 1 {
		t.Fatalf("stall reports = %d, want 1", len(rec.traces))
	}
	trace := rec.traces[0]
	if len(trace) != traceLen {
		t.Fatalf("trace length = %d, want %d", len(trace), traceLen)
	}
	if first := trace[0].String(); !strings.HasPrefix(first, "prepare_transition t9 ") {
		t.Errorf("oldest kept event = %q, want transition 9", first)
	}
	if last := trace[len(trace)-1].String(); !strings.HasPrefix(last, "prepare_transition t40 ") {
		t.Errorf("newest event = %q, want transition 40", last)
	}
}
