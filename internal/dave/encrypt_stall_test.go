package dave

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/disgoorg/godave"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// errMissingKeyRatchet matches libdave's ErrMissingKeyRatchet by text; the CGO
// package itself cannot be imported here.
var errMissingKeyRatchet = errors.New("missing key ratchet")

// encryptingSession stands in for a golibdave session whose encryptor was left
// without a key ratchet: every Encrypt fails until the next transition.
type encryptingSession struct {
	godave.Session
	err error
}

func (s *encryptingSession) Encrypt(_ uint32, _ []byte, _ []byte) (int, error) {
	return 0, s.err
}

// fakeConn stands in for the voice.Conn disgo passes as the session callbacks.
type fakeConn struct{ godave.Callbacks }

type stallRecorder struct {
	mu    sync.Mutex
	calls []godave.UserID
	conns []godave.Callbacks
}

func (r *stallRecorder) notify(selfUserID godave.UserID, conn godave.Callbacks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, selfUserID)
	r.conns = append(r.conns, conn)
}

// Production, 2026-10-10 and 2026-10-11: a member leaving the channel left one
// speaker's encryptor without a key ratchet. Every outbound frame then failed
// with "missing key ratchet" for up to an hour — the bot was silent in its
// channel while looking connected — until an unrelated membership change
// happened to repair it. A stalled encryptor has to ask for a rejoin.
func TestStalledEncryptorRequestsRejoin(t *testing.T) {
	const stallFrames = 500 // 10 s of speech at 20 ms per frame

	backend := &encryptingSession{err: errMissingKeyRatchet}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	rec := &stallRecorder{}
	conn := &fakeConn{}

	session := Instrument(inner, NewStats(), WithEncryptStall(stallFrames, rec.notify))(
		slog.Default(), "speaker-1", conn)

	for range stallFrames {
		if _, err := session.Encrypt(1, []byte("opus"), make([]byte, 64)); err == nil {
			t.Fatal("expected the backend error to pass through")
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("rejoin requests after %d consecutive encrypt failures = %d, want 1: "+
			"the bot stays silent in its channel until someone else joins or leaves",
			stallFrames, len(rec.calls))
	}
	if rec.calls[0] != "speaker-1" {
		t.Errorf("rejoin requested for %q, want speaker-1", rec.calls[0])
	}
	if rec.conns[0] != conn {
		t.Errorf("rejoin requested for conn %v, want the session's own conn", rec.conns[0])
	}
}

func stallSession(backend *encryptingSession, frames int, notify EncryptStallFunc) godave.Session {
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	return Instrument(inner, NewStats(), WithEncryptStall(frames, notify))(slog.Default(), "speaker-1", &fakeConn{})
}

func encryptN(session godave.Session, n int) {
	for range n {
		_, _ = session.Encrypt(1, []byte("opus"), make([]byte, 64))
	}
}

// Encrypt failures scattered through healthy audio are not a silent bot.
func TestEncryptSuccessResetsTheStallCount(t *testing.T) {
	backend := &encryptingSession{err: errMissingKeyRatchet}
	rec := &stallRecorder{}
	session := stallSession(backend, 10, rec.notify)

	encryptN(session, 9)
	backend.err = nil
	encryptN(session, 1)
	backend.err = errMissingKeyRatchet
	encryptN(session, 9)

	if len(rec.calls) != 0 {
		t.Fatalf("rejoin requests = %d, want 0: no run of 10 consecutive failures happened", len(rec.calls))
	}
}

// A rejoin can be skipped (another reconnect in flight) or fail; a session that
// is still stalled must ask again rather than once for its lifetime.
func TestContinuingStallIsReportedAgain(t *testing.T) {
	backend := &encryptingSession{err: errMissingKeyRatchet}
	rec := &stallRecorder{}
	session := stallSession(backend, 10, rec.notify)

	encryptN(session, 25)

	if len(rec.calls) != 2 {
		t.Fatalf("rejoin requests after 25 failures = %d, want 2 (at 10 and 20)", len(rec.calls))
	}
}

func TestEncryptStallsForwardsToTheInstalledHandler(t *testing.T) {
	stalls := NewEncryptStalls()
	stalls.Notify("speaker-1", nil) // before a handler: dropped, must not panic

	rec := &stallRecorder{}
	stalls.SetHandler(rec.notify)
	stalls.Notify("speaker-1", nil)

	if len(rec.calls) != 1 {
		t.Fatalf("handler calls = %d, want 1", len(rec.calls))
	}
}

// encryptPoints runs one collection and returns the encrypt counter's points.
func encryptPoints(t *testing.T, reader *metric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.dave.encrypt.total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gdc.dave.encrypt.total is %T, want Sum[int64]", md.Data)
			}
			return sum.DataPoints
		}
	}
	return nil
}

func encryptCount(points []metricdata.DataPoint[int64], botUserID, outcome, reason string) int64 {
	for _, p := range points {
		b, _ := p.Attributes.Value("bot_user_id")
		o, _ := p.Attributes.Value("outcome")
		r, _ := p.Attributes.Value("reason")
		if b.AsString() == botUserID && o.AsString() == outcome && r.AsString() == reason {
			return p.Value
		}
	}
	return -1
}

// A bot that cannot encrypt is silent while looking connected, and disgo's
// per-frame log line is off by default, so the counter is the only way to see
// it — per bot, with the reason.
func TestEncryptOutcomesAreCountedPerBot(t *testing.T) {
	stats := NewStats()
	backend := &encryptingSession{}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	session := Instrument(inner, stats)(slog.Default(), "speaker-1", &fakeConn{})

	encryptN(session, 3)
	backend.err = errMissingKeyRatchet
	encryptN(session, 2)

	reader, _ := newReader(t, stats)
	pts := encryptPoints(t, reader)
	if got := encryptCount(pts, "speaker-1", outcomeSuccess, reasonNone); got != 3 {
		t.Errorf("successful encrypts = %d, want 3", got)
	}
	if got := encryptCount(pts, "speaker-1", outcomeFailure, "missing_key_ratchet"); got != 2 {
		t.Errorf("missing-key-ratchet encrypts = %d, want 2", got)
	}
}

// The bot is the only sender of its own frames: encrypt outcomes must not be
// counted as a decrypt sender, or a silent bot would show up as a "failing"
// participant in the sender-health gauge.
func TestEncryptDoesNotCountAsADecryptSender(t *testing.T) {
	stats := NewStats()
	backend := &encryptingSession{err: errMissingKeyRatchet}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	session := Instrument(inner, stats)(slog.Default(), "speaker-1", &fakeConn{})

	encryptN(session, 5)

	reader, _ := newReader(t, stats)
	counts, gauge := collectOnce(t, reader)
	if len(counts) != 0 || len(gauge) != 0 {
		t.Errorf("decrypt series after encrypts only: %d counter points, %d sender points; want none", len(counts), len(gauge))
	}
}
