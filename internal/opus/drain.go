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
	mixer     *Mixer
	idle      time.Duration
	keepAlive func() bool
	// pausedByWatcher records that the pause currently in effect is this
	// watcher's doing, so Run only ever reverses its own decision. Touched
	// solely from the Run goroutine.
	pausedByWatcher bool
}

// NewDrainWatcher creates a DrainWatcher for mx with the given idle threshold.
func NewDrainWatcher(mx *Mixer, idle time.Duration) *DrainWatcher {
	return &DrainWatcher{mixer: mx, idle: idle}
}

// WithKeepAlive marks the mixer as one this watcher must never auto-pause while
// fn returns true. Pass the destination's relay-feed predicate: a cross-guild
// relay input can deliver a frame at any moment, and idle time is not evidence
// that such a mixer can be parked — a paused mixer drains its inputs WITHOUT
// recording activity (Mixer.collectFrames), so once auto-paused during a lull it
// could never wake itself again and every relayed packet was dropped from then
// on (issue #51). Pause decisions for these mixers belong to the router, which
// owns both the cascade and the listener check. nil-safe; returns w.
func (w *DrainWatcher) WithKeepAlive(fn func() bool) *DrainWatcher {
	w.keepAlive = fn
	return w
}

// Run checks mixer activity every idle/2 until ctx is cancelled.
// Pauses the mixer when idle; unpauses when frames resume — so a caller who
// simply stops talking for a while, without any voice-state event to trigger a
// router Recompute, is handled automatically.
// Mixers held live by WithKeepAlive are left exactly as the router set them.
//
// Pause ownership matters here. The router pauses destinations that have no
// human listener (or nothing to mix), and that decision must not be undone
// just because audio is still arriving — so Run reverses a pause only when it
// was the one that applied it. Anything else it observes belongs to the router
// and is left alone.
func (w *DrainWatcher) Run(ctx context.Context) {
	t := time.NewTicker(w.idle / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.keepAlive != nil && w.keepAlive() {
				continue
			}
			w.step()
		}
	}
}

// step applies one pause/unpause decision. Split out so tests can drive it
// deterministically instead of racing the ticker.
func (w *DrainWatcher) step() {
	idle := w.mixer.IdleFor() > w.idle
	paused := w.mixer.Paused()

	// The router unpaused it (or never paused it): drop our claim.
	if !paused {
		w.pausedByWatcher = false
		if idle {
			w.mixer.SetPaused(true)
			w.pausedByWatcher = true
		}
		return
	}

	// Paused. Only lift it if the pause is ours and audio has resumed;
	// a router pause stays put no matter how busy the inputs are.
	if w.pausedByWatcher && !idle {
		w.mixer.SetPaused(false)
		w.pausedByWatcher = false
	}
}
