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
	outcomeSuccess     = "success"
	outcomePassthrough = "passthrough"
	outcomeDropped     = "dropped"
	outcomeFailure     = "failure"

	reasonNone = "none"
)

// UnknownUserID is what disgo passes for an SSRC it has no SPEAKING op for.
// No decryptor exists for it, so those frames are passed through rather than
// decrypted — and the backend reports no error, making them invisible in the
// logs.
const UnknownUserID = "0"

// key identifies one counter series. All fields are label values.
type key struct {
	botUserID string
	userID    string
	outcome   string
	reason    string
}

// user identifies every series belonging to one sender on one bot, which is
// the granularity the backend adds and removes them at.
type user struct {
	botUserID string
	userID    string
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
// Series are retired when the last session drops the user, because a voice
// channel sees an unbounded number of distinct speakers over the life of the
// process and nothing else would ever remove them — the counters would
// accumulate in memory and as billed series for as long as the bot runs.
// Retirement is deferred by one collection so the final values are still
// exported; a user who rejoins before that keeps their counters, and one who
// rejoins later starts a new series from zero, which reads as an ordinary
// counter reset.
//
// "The last session" is why holders are counted rather than just flagged: one
// bot runs one DAVE session per voice connection — the owner bot has one per
// guest guild plus the host — and they all share this Stats, aggregated under
// one series per sender. Retiring on the first leave would delete counters a
// still-live session keeps incrementing, exporting a reset that never happened.
type Stats struct {
	mu       sync.RWMutex
	counters map[key]*atomic.Uint64
	holders  map[user]int
	retiring map[user]struct{}
	metrics  *telemetry.DaveMetrics
}

func NewStats() *Stats {
	return &Stats{
		counters: make(map[key]*atomic.Uint64),
		holders:  make(map[user]int),
		retiring: make(map[user]struct{}),
	}
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

// Record counts one Decrypt call that reached the backend. err is the error it
// returned, or nil.
func (s *Stats) Record(botUserID, userID string, err error) {
	s.counter(key{
		botUserID: botUserID,
		userID:    userID,
		outcome:   outcome(err),
		reason:    reason(err),
	}).Add(1)
}

// RecordPassthrough counts one frame from a user the backend has no decryptor
// for, which is forwarded as-is rather than decrypted.
func (s *Stats) RecordPassthrough(botUserID, userID string) {
	s.counter(key{
		botUserID: botUserID,
		userID:    userID,
		outcome:   outcomePassthrough,
		reason:    reasonNone,
	}).Add(1)
}

// RecordDropped counts one frame discarded because there was no decryptor for
// the sender while the group was encrypted, so the frame could only be
// ciphertext.
func (s *Stats) RecordDropped(botUserID, userID string) {
	s.counter(key{
		botUserID: botUserID,
		userID:    userID,
		outcome:   outcomeDropped,
		reason:    reasonNone,
	}).Add(1)
}

// Retire releases one session's hold on a user and, once none are left, marks
// their series for removal after the next collection exports the final values.
// Call when a session drops the user, exactly once per Keep.
func (s *Stats) Retire(botUserID, userID string) {
	u := user{botUserID: botUserID, userID: userID}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.holders[u]--
	if s.holders[u] > 0 {
		return
	}
	delete(s.holders, u)
	s.retiring[u] = struct{}{}
}

// Keep takes a hold on a user's series and cancels any pending retirement. Call
// when a session adds the user, so a rejoin before the next collection
// continues the existing series instead of resetting it.
func (s *Stats) Keep(botUserID, userID string) {
	u := user{botUserID: botUserID, userID: userID}

	s.mu.Lock()
	s.holders[u]++
	delete(s.retiring, u)
	s.mu.Unlock()
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
	for k, c := range s.counters {
		s.metrics.ObserveDecrypt(o, k.botUserID, k.userID, k.outcome, k.reason, int64(c.Load()))
	}
	s.mu.RUnlock()

	s.evictRetired()
	return nil
}

// evictRetired drops the series of every user retired before this collection.
// Their final values have just been exported.
func (s *Stats) evictRetired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.retiring) == 0 {
		return
	}
	for k := range s.counters {
		if _, ok := s.retiring[user{botUserID: k.botUserID, userID: k.userID}]; ok {
			delete(s.counters, k)
		}
	}
	clear(s.retiring)
}

func outcome(err error) string {
	if err != nil {
		return outcomeFailure
	}
	return outcomeSuccess
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
