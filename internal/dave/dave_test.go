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
type fakeSession struct {
	godave.Session
	err error
}

func (f *fakeSession) Decrypt(_ godave.UserID, _ []byte, _ []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return 42, nil
}

func newTestSession(t *testing.T, stats *Stats, selfUserID string, err error) godave.Session {
	t.Helper()
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
		return &fakeSession{err: err}
	}
	return Instrument(inner, stats)(slog.Default(), godave.UserID(selfUserID), nil)
}

// collect runs one metric collection and returns the decrypt counter points.
func collect(t *testing.T, stats *Stats) []metricdata.DataPoint[int64] {
	t.Helper()

	reader := metric.NewManualReader()
	m, err := telemetry.NewMetrics(metric.NewMeterProvider(metric.WithReader(reader)).Meter("dave_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	stats.StartMetrics(&m.Dave)

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
	t.Fatal("gdc.dave.decrypt.total was not collected")
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
	broken := newTestSession(t, stats, "bot-1", errors.New("failed to decrypt frame"))
	working := newTestSession(t, stats, "bot-1", nil)

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
	session := newTestSession(t, stats, "bot-1", nil)

	if _, err := session.Decrypt(godave.UserID(UnknownUserID), nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	points := collect(t, stats)
	if got := find(t, points, UnknownUserID, outcomeUnknown); got != 1 {
		t.Errorf("unknown-user count = %d, want 1", got)
	}
	if got := find(t, points, UnknownUserID, outcomeSuccess); got != -1 {
		t.Errorf("unknown user must not be counted as success, got %d", got)
	}
}

func TestDecryptPassesBackendResultThrough(t *testing.T) {
	stats := NewStats()
	session := newTestSession(t, stats, "bot-1", nil)

	n, err := session.Decrypt("user-a", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 42 {
		t.Errorf("n = %d, want the backend's 42", n)
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
			session := newTestSession(t, stats, "bot-1", nil)
			for range 100 {
				_, _ = session.Decrypt(godave.UserID("user-a"), nil, nil)
			}
		}(i)
	}
	wg.Wait()

	if got := find(t, collect(t, stats), "user-a", "success"); got != 800 {
		t.Errorf("success count = %d, want 800", got)
	}
}
