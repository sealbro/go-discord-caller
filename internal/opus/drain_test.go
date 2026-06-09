package opus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProbe struct {
	paused atomic.Bool
}

func (f *fakeProbe) Paused() bool { return f.paused.Load() }

// TestSessionIdleWatcherCancelsWhenAllPaused checks that the watcher fires
// cancelFunc once every probe has been paused continuously past idleTimeout.
func TestSessionIdleWatcherCancelsWhenAllPaused(t *testing.T) {
	t.Parallel()

	a := &fakeProbe{}
	b := &fakeProbe{}
	a.paused.Store(true)
	b.paused.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cancelled := make(chan struct{})
	idleTimeout := 1500 * time.Millisecond // interval clamps to 1s; one full window
	w := NewSessionIdleWatcher([]PauseProbe{a, b}, func() { close(cancelled) }, idleTimeout)
	go w.Run(ctx)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not cancel after all probes stayed paused")
	}
}

// TestSessionIdleWatcherNoCancelWhenActive checks that an unpaused probe
// keeps the session alive — pausedSince must reset on any active probe.
func TestSessionIdleWatcherNoCancelWhenActive(t *testing.T) {
	t.Parallel()

	a := &fakeProbe{}
	b := &fakeProbe{}
	a.paused.Store(true)
	// b stays unpaused → watcher should never cancel.

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cancelled := make(chan struct{})
	w := NewSessionIdleWatcher([]PauseProbe{a, b}, func() { close(cancelled) }, time.Second)
	go w.Run(ctx)

	select {
	case <-cancelled:
		t.Fatal("watcher cancelled despite an active probe")
	case <-time.After(2500 * time.Millisecond):
	}
}

// TestSessionIdleWatcherDisabled checks that idleTimeout <= 0 makes Run a no-op.
func TestSessionIdleWatcherDisabled(t *testing.T) {
	t.Parallel()

	a := &fakeProbe{}
	a.paused.Store(true)

	cancelled := make(chan struct{})
	w := NewSessionIdleWatcher([]PauseProbe{a}, func() { close(cancelled) }, 0)
	done := make(chan struct{})
	go func() {
		w.Run(t.Context())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return immediately when idleTimeout <= 0")
	}
	select {
	case <-cancelled:
		t.Fatal("cancelFunc invoked when watcher disabled")
	default:
	}
}

// TestSessionIdleWatcherEmpty checks that an empty mixers slice makes Run a no-op.
func TestSessionIdleWatcherEmpty(t *testing.T) {
	t.Parallel()

	cancelled := make(chan struct{})
	w := NewSessionIdleWatcher(nil, func() { close(cancelled) }, time.Second)
	done := make(chan struct{})
	go func() {
		w.Run(t.Context())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return immediately when no probes")
	}
	select {
	case <-cancelled:
		t.Fatal("cancelFunc invoked with no probes")
	default:
	}
}

// TestSessionIdleWatcherResetsOnUnpause checks that pausedSince resets when a
// probe transitions back to unpaused — i.e. activity bumps the watcher window.
func TestSessionIdleWatcherResetsOnUnpause(t *testing.T) {
	t.Parallel()

	a := &fakeProbe{}
	a.paused.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cancelled := make(chan struct{})
	idleTimeout := 2500 * time.Millisecond
	w := NewSessionIdleWatcher([]PauseProbe{a}, func() { close(cancelled) }, idleTimeout)
	go w.Run(ctx)

	// Halfway through the window, unpause briefly so pausedSince resets.
	time.Sleep(1500 * time.Millisecond)
	a.paused.Store(false)
	time.Sleep(1200 * time.Millisecond)
	a.paused.Store(true)

	// At this point the original window would have elapsed (1500+1200=2700ms > 2500),
	// but the unpause should have reset pausedSince — so cancel must NOT have fired yet.
	select {
	case <-cancelled:
		t.Fatal("watcher cancelled despite mid-window activity resetting pausedSince")
	default:
	}
}
