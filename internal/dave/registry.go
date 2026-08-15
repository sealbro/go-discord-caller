package dave

import (
	"context"
	"sync"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
	davego "github.com/thomas-vilte/dave-go/session"
	"go.opentelemetry.io/otel/metric"
)

// Registry tracks the live dave-go sessions of this process so their counters
// can be exported and their watchdogs shut down.
//
// It exists because disgo owns the DAVE session end to end: voice.NewConn
// builds one per connection through the configured SessionCreateFunc and never
// exposes it again, and (as of v0.19.6) never calls Close on it either. With
// golibdave that is harmless — its Close is a documented no-op, the native
// objects are freed by finalizers. dave-go instead runs recovery and
// commit-confirm watchdogs that only stop on Close; left alone after the
// connection is discarded they keep re-arming invalidations and sending
// InvalidCommitWelcome against a channel the bot no longer occupies for up to
// ~45s. So the session hook stashes every session here, keyed by the
// godave.Callbacks value disgo passes to the factory — which is the *connImpl
// itself, i.e. the same dynamic value the voice.Conn interface holds. Release
// then takes a voice.Conn and closes exactly that session.
//
// Sessions are per voice connection and short-lived, so their counters are
// never exported per session: that would churn one time series per join.
// Snapshot sums the live sessions and adds the final values of every session
// released so far, which keeps the totals monotonic across the churn.
//
// A nil *Registry is usable and does nothing — that is the libdave case, where
// no session is ever tracked.
type Registry struct {
	mu      sync.Mutex
	live    map[any]*davego.Session
	retired telemetry.DaveObservation // final counters of released sessions
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{live: make(map[any]*davego.Session)}
}

// Track records a session under key. Called from the dave-go session hook, on
// the goroutine that opened the voice connection. If key already holds a
// session (disgo built a second connection for the same guild without the
// first being released), the previous one is closed and retired rather than
// leaked.
func (r *Registry) Track(key any, s *davego.Session) {
	if r == nil || s == nil || key == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.live[key]; ok {
		_ = old.Close()
		addStats(&r.retired, old.Stats())
	}
	r.live[key] = s
}

// Release closes and retires the session tracked under key, folding its final
// counters into the retired totals so Snapshot stays monotonic. Safe to call
// for a key that was never tracked (the libdave case, or a connection closed
// twice), and safe on a nil Registry.
func (r *Registry) Release(key any) {
	if r == nil || key == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.live[key]
	if !ok {
		return
	}
	delete(r.live, key)
	_ = s.Close()
	addStats(&r.retired, s.Stats())
}

// CloseAll releases every tracked session. Called on process shutdown, and the
// backstop for connections torn down by paths that never reach Release — a
// speaker client being closed outright, for instance, takes its voice
// connections with it without going through GuildVoice.Leave.
func (r *Registry) CloseAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, s := range r.live {
		delete(r.live, key)
		_ = s.Close()
		addStats(&r.retired, s.Stats())
	}
}

// addStats folds one session's counters into an aggregate.
func addStats(agg *telemetry.DaveObservation, st davego.Stats) {
	agg.CommitsProcessed += st.CommitsProcessed
	agg.CommitsFailed += st.CommitsFailed
	agg.WelcomesJoined += st.WelcomesJoined
	agg.WelcomesFailed += st.WelcomesFailed
	agg.RecoveriesMLS += st.RecoveryAttempts
	agg.RecoveriesTransport += st.RecoveryAttemptsTransport
	agg.EncryptFailures += st.EncryptFailures
	agg.DecryptFailures += st.DecryptFailures
	agg.PassthroughFrames += st.PassthroughFrames
	agg.TransitionFrames += st.TransitionFrames
	agg.TransitionWindows += st.TransitionWindows
	agg.ReplayRejected += st.RejectedReplayFrames
	agg.ProposalsRejected += st.ProposalsRejected
	agg.Downgrades += st.DowngradeToV0
	agg.DegradedSeconds += st.DegradedDuration.Seconds()
	agg.TransportRetrySeconds += st.TransportRetryDuration.Seconds()
}

// Snapshot returns the retired totals plus every live session's current
// counters, and counts the live sessions by health.
func (r *Registry) Snapshot() telemetry.DaveObservation {
	if r == nil {
		return telemetry.DaveObservation{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.retired
	for _, s := range r.live {
		addStats(&out, s.Stats())
		out.SessionsLive++
		// A session that never negotiates E2EE (protocol version 0) is not
		// degraded — it is a channel where DAVE will never activate, and it
		// stays !Ready forever. Only count sessions that expect E2EE and do
		// not currently have it.
		if st := s.State(); !st.Ready && st.ProtocolVersion > 0 {
			out.SessionsDegraded++
		}
	}
	return out
}

// StartMetrics registers the observable callback that exports the aggregated
// dave-go counters. Call once, after the pool is built. Registering is skipped
// for a nil Registry so the libdave path emits no dave-go series at all rather
// than a flat line of zeros.
func (r *Registry) StartMetrics(m *telemetry.DaveMetrics) error {
	if r == nil {
		return nil
	}
	return m.RegisterObservers(func(_ context.Context, o metric.Observer) error {
		m.Observe(o, r.Snapshot())
		return nil
	})
}
