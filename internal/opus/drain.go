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
// Pauses the mixer when idle; unpauses when frames resume — so a user who
// reconnects without triggering a voice-state event is handled automatically.
// Mixers held live by WithKeepAlive are left exactly as the router set them.
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
			w.mixer.SetPaused(w.mixer.IdleFor() > w.idle)
		}
	}
}
