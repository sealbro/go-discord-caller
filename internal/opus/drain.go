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

// PauseProbe is the subset of Mixer needed by SessionIdleWatcher. *Mixer
// satisfies it via its Paused() method.
type PauseProbe interface {
	Paused() bool
}

// SessionIdleWatcher cancels a voice raid session when every channel mixer has
// been paused continuously for idleTimeout. DrainWatcher already pauses an
// individual mixer after 5 s of no input; SessionIdleWatcher escalates that
// signal one level up — a session in which every destination channel mixer is
// quiet is a session nobody is using, so the watcher invokes cancelFunc to
// release its voice gateway slots.
type SessionIdleWatcher struct {
	mixers      []PauseProbe
	cancelFunc  context.CancelFunc
	idleTimeout time.Duration
}

// NewSessionIdleWatcher creates a SessionIdleWatcher. Run is a no-op when
// idleTimeout <= 0 or mixers is empty.
func NewSessionIdleWatcher(mixers []PauseProbe, cancelFunc context.CancelFunc, idleTimeout time.Duration) *SessionIdleWatcher {
	return &SessionIdleWatcher{mixers: mixers, cancelFunc: cancelFunc, idleTimeout: idleTimeout}
}

// Run polls until ctx is cancelled or until every mixer has been paused
// continuously for w.idleTimeout — in which case it calls cancelFunc and
// returns. Disabled when idleTimeout <= 0 or no mixers are provided.
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
	var pausedSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			allPaused := true
			for _, m := range w.mixers {
				if !m.Paused() {
					allPaused = false
					break
				}
			}
			if !allPaused {
				pausedSince = time.Time{}
				continue
			}
			if pausedSince.IsZero() {
				pausedSince = time.Now()
				continue
			}
			if time.Since(pausedSince) >= w.idleTimeout {
				w.cancelFunc()
				return
			}
		}
	}
}
