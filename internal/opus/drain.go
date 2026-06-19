package opus

import (
	"context"
	"time"
)

// DrainIdleTimeout is the duration of inactivity after which a channel mixer is
// automatically paused. Complements the event-driven pause path
// (manager.syncMixerPauseState) for cases where a user drops from voice without
// a clean disconnect event.
const DrainIdleTimeout = 5 * time.Second

// DrainWatcher auto-pauses a Mixer when no input frames arrive for DrainIdleTimeout,
// and auto-unpauses when activity resumes. One goroutine per channel mixer;
// typically 1–3 per active session.
type DrainWatcher struct {
	mixer *Mixer
	idle  time.Duration
}

// NewDrainWatcher creates a DrainWatcher for mx with the given idle threshold.
func NewDrainWatcher(mx *Mixer, idle time.Duration) *DrainWatcher {
	return &DrainWatcher{mixer: mx, idle: idle}
}

// Run checks mixer activity every idle/2 until ctx is cancelled.
// Pauses the mixer when idle; unpauses when frames resume — so a user who
// reconnects without triggering a voice-state event is handled automatically.
func (w *DrainWatcher) Run(ctx context.Context) {
	t := time.NewTicker(w.idle / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.mixer.SetPaused(w.mixer.IdleFor() > w.idle)
		}
	}
}

// IdleProbe is the subset of Mixer needed by SessionIdleWatcher. *Mixer
// satisfies it via Paused() and IdleFor().
//
// A mixer counts as "quiet" when EITHER condition holds:
//   - Paused() — the router parked it because the destination has no human
//     listeners or the cascade has nothing to mix (presence-driven).
//   - IdleFor() >= quietThreshold — no input frame has been consumed for a
//     while (activity-driven).
//
// Both signals are needed. Per-channel mixers get a DrainWatcher that flips
// Paused() on silence, but the synthetic relay mixer does NOT: its pause state
// is set only by the router on voice-state events, so a session whose callers
// stay in the channel but go silent leaves the relay mixer unpaused forever.
// Probing IdleFor() catches that case without force-pausing the relay mixer
// (which would risk dropping guest audio, since a DrainWatcher-paused mixer
// cannot resume until the router recomputes on a voice event).
type IdleProbe interface {
	Paused() bool
	IdleFor() time.Duration
}

// SessionIdleWatcher cancels a voice raid session when every mixer has been
// quiet continuously for idleTimeout. DrainWatcher already pauses an individual
// channel mixer after 5 s of no input; SessionIdleWatcher escalates that signal
// one level up — a session in which every destination mixer is quiet is a
// session nobody is using, so the watcher invokes cancelFunc to release its
// voice gateway slots.
type SessionIdleWatcher struct {
	mixers         []IdleProbe
	cancelFunc     context.CancelFunc
	idleTimeout    time.Duration
	quietThreshold time.Duration
}

// NewSessionIdleWatcher creates a SessionIdleWatcher. Run is a no-op when
// idleTimeout <= 0 or mixers is empty.
func NewSessionIdleWatcher(mixers []IdleProbe, cancelFunc context.CancelFunc, idleTimeout time.Duration) *SessionIdleWatcher {
	// A mixer is treated as activity-idle once it has gone DrainIdleTimeout
	// without consuming a frame — same threshold the per-channel DrainWatcher
	// uses to pause. Clamp so it never exceeds idleTimeout (keeps small-timeout
	// configs, and tests, well-behaved).
	quiet := DrainIdleTimeout
	if idleTimeout > 0 && quiet > idleTimeout {
		quiet = idleTimeout
	}
	return &SessionIdleWatcher{mixers: mixers, cancelFunc: cancelFunc, idleTimeout: idleTimeout, quietThreshold: quiet}
}

// quiet reports whether a mixer is idle: paused by the router, or no input
// frame consumed for at least quietThreshold.
func (w *SessionIdleWatcher) quiet(m IdleProbe) bool {
	return m.Paused() || m.IdleFor() >= w.quietThreshold
}

// idleUnpauseHysteresis is the number of consecutive "any mixer active"
// observations required to reset the quiet-since timer. The router can flap
// a mixer paused→unpaused→paused inside a single tick window (e.g. a burst
// of voice join/leave events); requiring two consecutive active ticks
// before reset prevents that flap from indefinitely deferring the auto-stop.
const idleUnpauseHysteresis = 2

// Run polls until ctx is cancelled or until every mixer has been quiet
// continuously for w.idleTimeout — in which case it calls cancelFunc and
// returns. Disabled when idleTimeout <= 0 or no mixers are provided.
//
// Hysteresis: a single tick where one mixer is active does NOT reset the
// quiet-since timer. Two consecutive active observations are required.
// This stops short-lived router transitions (cascade flips on a burst of
// voice events) from preventing the auto-stop indefinitely.
func (w *SessionIdleWatcher) Run(ctx context.Context) {
	if w.idleTimeout <= 0 || len(w.mixers) == 0 {
		return
	}
	// Poll roughly 10× over the timeout window so the auto-stop lag is bounded
	// at ~idleTimeout/10. Clamp to [1s, 1m] to keep ticker cost negligible for
	// any sensible timeout.
	interval := w.idleTimeout / 10
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var quietSince time.Time
	var activeCount int
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			allQuiet := true
			for _, m := range w.mixers {
				if !w.quiet(m) {
					allQuiet = false
					break
				}
			}
			if !allQuiet {
				activeCount++
				if activeCount >= idleUnpauseHysteresis {
					quietSince = time.Time{}
					activeCount = 0
				}
				continue
			}
			activeCount = 0
			if quietSince.IsZero() {
				quietSince = time.Now()
				continue
			}
			if time.Since(quietSince) >= w.idleTimeout {
				w.cancelFunc()
				return
			}
		}
	}
}
