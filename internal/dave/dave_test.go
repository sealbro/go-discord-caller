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

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// fakeSession stands in for the libdave backend. Only Decrypt is exercised, so
// the rest of the interface is left nil.
// fakeSession stands in for golibdave, reproducing the parts of v0.3.0 that
// matter here: it keeps its own set of users with decryptors, and answers a
// user outside that set on the passthrough branch whose copy arguments are
// swapped (golibdave.go:104).
type fakeSession struct {
	godave.Session
	err error

	// ready mirrors golibdave's Ready: false while the session is in
	// passthrough mode, true once an E2EE epoch is active.
	ready bool
	// onAddUser runs at the start of AddUser, before the decryptor exists, to
	// stand in for a frame arriving during libdave's CGO setup work.
	onAddUser func()

	mu       sync.Mutex
	decrypts int
	known    map[godave.UserID]struct{}
}

func (f *fakeSession) Ready() bool { return f.ready }

func (f *fakeSession) Decrypt(userID godave.UserID, frame []byte, out []byte) (int, error) {
	f.mu.Lock()
	f.decrypts++
	_, known := f.known[userID]
	f.mu.Unlock()

	if !known {
		return copy(frame, out), nil // upstream's swapped arguments, verbatim
	}
	if f.err != nil {
		return 0, f.err
	}
	copy(out, []byte("decrypted"))
	return 42, nil
}

func (f *fakeSession) AddUser(userID godave.UserID) {
	if f.onAddUser != nil {
		f.onAddUser()
	}
	f.mu.Lock()
	f.known[userID] = struct{}{}
	f.mu.Unlock()
}

func (f *fakeSession) RemoveUser(userID godave.UserID) {
	f.mu.Lock()
	delete(f.known, userID)
	f.mu.Unlock()
}

func (f *fakeSession) decryptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decrypts
}

func newTestSession(t *testing.T, stats *Stats, selfUserID string, err error) (godave.Session, *fakeSession) {
	t.Helper()
	backend := &fakeSession{err: err, known: make(map[godave.UserID]struct{})}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
		return backend
	}
	return Instrument(inner, stats)(slog.Default(), godave.UserID(selfUserID), nil), backend
}

// knownSession returns a session that already has a decryptor for each user, so
// Decrypt reaches the backend instead of the passthrough guard.
func knownSession(t *testing.T, stats *Stats, selfUserID string, err error, users ...string) (godave.Session, *fakeSession) {
	t.Helper()
	session, backend := newTestSession(t, stats, selfUserID, err)
	for _, u := range users {
		session.AddUser(godave.UserID(u))
	}
	return session, backend
}

// newReader attaches stats to a fresh manual reader.
func newReader(t *testing.T, stats *Stats) (*metric.ManualReader, *telemetry.Metrics) {
	t.Helper()

	reader := metric.NewManualReader()
	m, err := telemetry.NewMetrics(metric.NewMeterProvider(metric.WithReader(reader)).Meter("dave_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	stats.StartMetrics(&m.Dave)
	return reader, m
}

// collect runs one metric collection and returns the decrypt counter points.
func collect(t *testing.T, stats *Stats) []metricdata.DataPoint[int64] {
	t.Helper()
	reader, _ := newReader(t, stats)
	return points(t, reader)
}

// points runs one collection on reader and returns the decrypt counter points.
func points(t *testing.T, reader *metric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.dave.decrypt.total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gdc.dave.decrypt.total is %T, want Sum[int64]", md.Data)
			}
			return sum.DataPoints
		}
	}
	return nil
}

func find(t *testing.T, points []metricdata.DataPoint[int64], userID, outcome string) int64 {
	t.Helper()
	for _, p := range points {
		u, _ := p.Attributes.Value("user_id")
		o, _ := p.Attributes.Value("outcome")
		if u.AsString() == userID && o.AsString() == outcome {
			return p.Value
		}
	}
	return -1
}

// The whole point of the instrument: tell one permanently failing sender apart
// from every sender losing a fraction of frames. disgo's log line cannot.
func TestDecryptFailuresAreAttributedToTheSendingUser(t *testing.T) {
	stats := NewStats()
	broken, _ := knownSession(t, stats, "bot-1", errors.New("failed to decrypt frame"), "user-a")
	working, _ := knownSession(t, stats, "bot-1", nil, "user-b")

	for range 3 {
		if _, err := broken.Decrypt("user-a", nil, nil); err == nil {
			t.Fatal("expected the backend error to pass through")
		}
	}
	for range 5 {
		if _, err := working.Decrypt("user-b", nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	points := collect(t, stats)
	if got := find(t, points, "user-a", "failure"); got != 3 {
		t.Errorf("user-a failures = %d, want 3", got)
	}
	if got := find(t, points, "user-b", "success"); got != 5 {
		t.Errorf("user-b successes = %d, want 5", got)
	}
	if got := find(t, points, "user-a", "success"); got != -1 {
		t.Errorf("user-a should have no successes, got %d", got)
	}
}

// An SSRC with no SPEAKING op decrypts as user "0", which golibdave answers
// with passthrough and no error — invisible in the logs, so it needs its own
// outcome rather than being counted as a success.
func TestUnknownUserIsNotCountedAsSuccess(t *testing.T) {
	stats := NewStats()
	session, _ := newTestSession(t, stats, "bot-1", nil)

	if _, err := session.Decrypt(godave.UserID(UnknownUserID), nil, make([]byte, 8)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	points := collect(t, stats)
	if got := find(t, points, UnknownUserID, outcomePassthrough); got != 1 {
		t.Errorf("passthrough count = %d, want 1", got)
	}
	if got := find(t, points, UnknownUserID, outcomeSuccess); got != -1 {
		t.Errorf("passthrough must not be counted as success, got %d", got)
	}
}

func TestDecryptPassesBackendResultThrough(t *testing.T) {
	stats := NewStats()
	session, _ := knownSession(t, stats, "bot-1", nil, "user-a")

	n, err := session.Decrypt("user-a", nil, make([]byte, 64))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 42 {
		t.Errorf("n = %d, want the backend's 42", n)
	}
}

// golibdave v0.3.0 answers an unknown user with a swapped-argument copy, which
// writes nothing and leaves the caller reading a frame of silence. The wrapper
// must do the copy itself rather than delegate.
func TestUnknownUserFrameIsCopiedNotSilenced(t *testing.T) {
	stats := NewStats()
	session, backend := newTestSession(t, stats, "bot-1", nil)

	frame := []byte("opus-audio")
	out := make([]byte, 0, 512) // len 0, cap grown — exactly how disgo passes it

	n, err := session.Decrypt("user-a", frame, out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(frame) {
		t.Fatalf("n = %d, want %d", n, len(frame))
	}
	if got := string(out[:cap(out)][:n]); got != string(frame) {
		t.Errorf("output = %q, want the frame %q — passthrough silenced the audio", got, frame)
	}
	if backend.decryptCalls() != 0 {
		t.Errorf("backend Decrypt was called %d times; the broken branch must not be reached", backend.decryptCalls())
	}
}

// A voice channel sees an unbounded number of distinct speakers over the life
// of the process; without eviction their counters accumulate forever, in memory
// and as billed series.
func TestRetiredUserSeriesAreEvictedAfterOneCollection(t *testing.T) {
	stats := NewStats()
	session, _ := knownSession(t, stats, "bot-1", nil, "user-a")

	if _, err := session.Decrypt("user-a", nil, make([]byte, 64)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	session.RemoveUser("user-a")

	reader, _ := newReader(t, stats)

	// First collection still reports the final value.
	if got := find(t, points(t, reader), "user-a", outcomeSuccess); got != 1 {
		t.Errorf("final value = %d, want 1 exported before eviction", got)
	}
	// Second collection no longer carries the series.
	if got := find(t, points(t, reader), "user-a", outcomeSuccess); got != -1 {
		t.Errorf("series should be evicted, still got %d", got)
	}
}

// A user who rejoins before the collection that would retire them keeps their
// counters, so the series does not reset for a brief reconnect.
func TestRejoinBeforeCollectionKeepsTheSeries(t *testing.T) {
	stats := NewStats()
	session, _ := knownSession(t, stats, "bot-1", nil, "user-a")

	if _, err := session.Decrypt("user-a", nil, make([]byte, 64)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	session.RemoveUser("user-a")
	session.AddUser("user-a")

	reader, _ := newReader(t, stats)
	_ = points(t, reader)

	if got := find(t, points(t, reader), "user-a", outcomeSuccess); got != 1 {
		t.Errorf("count after rejoin = %d, want the original 1", got)
	}
}

func TestReasonLabels(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, reasonNone},
		{errors.New("failed to decrypt frame"), "decrypt_failure"},
		{errors.New("failed to DAVE decrypt packet: missing key ratchet"), "missing_key_ratchet"},
		{errors.New("invalid nonce"), "invalid_nonce"},
		{errors.New("missing cryptor"), "missing_cryptor"},
		{errors.New("something new upstream"), "other"},
	}

	for _, tt := range tests {
		if got := reason(tt.err); got != tt.want {
			t.Errorf("reason(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// Decrypt runs on disgo's UDP read goroutine, one per voice connection, all
// sharing one Stats.
func TestRecordIsConcurrencySafe(t *testing.T) {
	stats := NewStats()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session, _ := knownSession(t, stats, "bot-1", nil, "user-a")
			for range 100 {
				_, _ = session.Decrypt(godave.UserID("user-a"), nil, make([]byte, 64))
			}
		}(i)
	}
	wg.Wait()

	if got := find(t, collect(t, stats), "user-a", "success"); got != 800 {
		t.Errorf("success count = %d, want 800", got)
	}
}

// While the DAVE group is live, a frame from a user the wrapper has no
// decryptor for is ciphertext. Copying it through hands the receiver, mixer and
// relay a garbage Opus packet; disgo produces user "0" whenever an RTP packet
// arrives before its SPEAKING op, which is routine on join.
func TestUnknownUserFrameIsDroppedWhileEncrypted(t *testing.T) {
	stats := NewStats()
	session, backend := newTestSession(t, stats, "bot-1", nil)
	backend.ready = true

	frame := []byte("ciphertext!")
	out := make([]byte, 0, 512)

	n, err := session.Decrypt(godave.UserID(UnknownUserID), frame, out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 — ciphertext must not be forwarded as audio", n)
	}
	if got := string(out[:cap(out)][:len(frame)]); got == string(frame) {
		t.Errorf("ciphertext was copied into the output buffer: %q", got)
	}
	if backend.decryptCalls() != 0 {
		t.Errorf("backend Decrypt was called %d times; the broken branch must not be reached", backend.decryptCalls())
	}

	points := collect(t, stats)
	if got := find(t, points, UnknownUserID, outcomeDropped); got != 1 {
		t.Errorf("dropped count = %d, want 1", got)
	}
	if got := find(t, points, UnknownUserID, outcomePassthrough); got != -1 {
		t.Errorf("an encrypted drop must not be counted as passthrough, got %d", got)
	}
}

// A frame can arrive while AddUser is still installing the backend's decryptor
// — libdave does CGO key-ratchet work there, and frames arrive 50/s per speaker
// on disgo's UDP goroutine. Until the backend can decrypt, the wrapper must
// keep handling the frame itself rather than delegate into golibdave's broken
// passthrough branch.
func TestFrameArrivingDuringAddUserDoesNotReachTheBackend(t *testing.T) {
	stats := NewStats()
	session, backend := newTestSession(t, stats, "bot-1", nil)
	backend.onAddUser = func() {
		if _, err := session.Decrypt("user-a", []byte("frame"), make([]byte, 0, 512)); err != nil {
			t.Errorf("unexpected error inside the AddUser window: %v", err)
		}
	}

	session.AddUser("user-a")

	if backend.decryptCalls() != 0 {
		t.Errorf("backend Decrypt was called %d times during the AddUser window; it has no decryptor yet", backend.decryptCalls())
	}
	if got := find(t, collect(t, stats), "user-a", outcomeSuccess); got != -1 {
		t.Errorf("a frame the backend could not decrypt was counted as success (%d)", got)
	}
}

// One bot holds one DAVE session per voice connection — the owner bot has one
// per guest guild plus the host — and they all share one process-wide Stats. A
// user leaving one of those channels must not retire the counters another live
// session is still incrementing, or the export drops to zero and restarts,
// which Prometheus reads as a counter reset.
func TestUserLeavingOneSessionKeepsCountersOfAnother(t *testing.T) {
	stats := NewStats()
	host, _ := knownSession(t, stats, "bot-1", nil, "user-a")
	guest, _ := knownSession(t, stats, "bot-1", nil, "user-a")

	if _, err := host.Decrypt("user-a", nil, make([]byte, 64)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	guest.RemoveUser("user-a")

	reader, _ := newReader(t, stats)
	_ = points(t, reader) // the collection that would export final values
	if got := find(t, points(t, reader), "user-a", outcomeSuccess); got != 1 {
		t.Errorf("count = %d, want 1 kept while the host session is still live", got)
	}

	host.RemoveUser("user-a")
	_ = points(t, reader)
	if got := find(t, points(t, reader), "user-a", outcomeSuccess); got != -1 {
		t.Errorf("series should be evicted once the last session drops the user, still got %d", got)
	}
}
