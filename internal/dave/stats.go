// Package dave instruments the DAVE end-to-end encryption layer.
//
// It stays free of CGO: the libdave backend is reached only through the
// godave.Session interface, so this package can be imported anywhere without
// dragging in the C toolchain.
package dave

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// Decrypt outcomes, reported as the outcome label.
const (
	outcomeSuccess = "success"
	outcomeUnknown = "unknown_user"
	outcomeFailure = "failure"

	reasonNone = "none"
)

// UnknownUserID is what disgo passes for an SSRC it has no SPEAKING op for.
// golibdave has no decryptor for it and falls back to passthrough, which
// returns no error, so these frames are invisible in the logs.
const UnknownUserID = "0"

// key identifies one counter series. All fields are label values.
type key struct {
	botUserID string
	userID    string
	outcome   string
	reason    string
}

// Stats aggregates decrypt outcomes in memory and reports them through an
// observable counter.
//
// Decrypt runs on disgo's UDP read goroutine, once per inbound packet — 50/s
// per speaking user, per bot. Counting there with the OTel API would allocate
// an attribute set per frame on the hottest path in the process, so the record
// path is an atomic increment on a preallocated counter and the OTel work
// happens once per collection interval instead.
//
// Stats is created before the meter exists (clients are built ahead of
// telemetry.NewMetrics), which is why the instrument is attached later by
// StartMetrics rather than passed to the constructor.
type Stats struct {
	mu       sync.RWMutex
	counters map[key]*atomic.Uint64
	metrics  *telemetry.DaveMetrics
}

func NewStats() *Stats {
	return &Stats{counters: make(map[key]*atomic.Uint64)}
}

// StartMetrics attaches m and registers the observable callback. Call once,
// after telemetry.NewMetrics.
func (s *Stats) StartMetrics(m *telemetry.DaveMetrics) {
	s.mu.Lock()
	s.metrics = m
	s.mu.Unlock()

	if err := m.RegisterObservers(s.observe); err != nil {
		slog.Error("dave: failed to register decrypt metrics callback", slog.Any("err", err))
	}
}

// Record counts one Decrypt call. err is the error it returned, or nil.
func (s *Stats) Record(botUserID, userID string, err error) {
	s.counter(key{
		botUserID: botUserID,
		userID:    userID,
		outcome:   outcome(userID, err),
		reason:    reason(err),
	}).Add(1)
}

func (s *Stats) counter(k key) *atomic.Uint64 {
	s.mu.RLock()
	c, ok := s.counters[k]
	s.mu.RUnlock()
	if ok {
		return c
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok = s.counters[k]; ok {
		return c
	}
	c = new(atomic.Uint64)
	s.counters[k] = c
	return c
}

func (s *Stats) observe(_ context.Context, o metric.Observer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for k, c := range s.counters {
		s.metrics.ObserveDecrypt(o, k.botUserID, k.userID, k.outcome, k.reason, int64(c.Load()))
	}
	return nil
}

func outcome(userID string, err error) string {
	switch {
	case err != nil:
		return outcomeFailure
	case userID == UnknownUserID:
		return outcomeUnknown
	default:
		return outcomeSuccess
	}
}

// reason maps a libdave decrypt error to a stable label value.
//
// The comparison is on the message rather than the sentinel errors, because
// those live in the libdave package and importing it here would make this
// package CGO-only. libdave's set is small and stable
// (libdave/errors.go): anything outside it lands in "other" rather than
// becoming an unbounded label.
func reason(err error) string {
	if err == nil {
		return reasonNone
	}

	msg := err.Error()
	for _, r := range []struct {
		substr string
		label  string
	}{
		{"missing key ratchet", "missing_key_ratchet"},
		{"invalid nonce", "invalid_nonce"},
		{"missing cryptor", "missing_cryptor"},
		{"failed to decrypt frame", "decrypt_failure"},
	} {
		if strings.Contains(msg, r.substr) {
			return r.label
		}
	}
	return "other"
}
