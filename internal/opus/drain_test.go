package opus

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeProbe models a mixer for SessionIdleWatcher: paused state plus a settable
// IdleFor (duration since the last consumed frame). idleNanos defaults to 0,
// i.e. "just consumed a frame" → active.
type fakeProbe struct {
	paused    atomic.Bool
	idleNanos atomic.Int64
}

func (f *fakeProbe) Paused() bool { return f.paused.Load() }

func (f *fakeProbe) IdleFor() time.Duration { return time.Duration(f.idleNanos.Load()) }

// The SessionIdleWatcher tests run inside testing/synctest bubbles so they use a
// fake clock: the watcher's ticker and every time.Now/time.Since advance in
// virtual time. This makes the previously sleep-based tests both deterministic
// and instant (the old versions slept 2.5–3 real seconds each), and lets us
// assert the exact cancel latency, which real-time tests could not.

// TestSessionIdleWatcherCancelsWhenAllPaused checks that the watcher fires
// cancelFunc once every probe has been quiet continuously past idleTimeout.
func TestSessionIdleWatcherCancelsWhenAllPaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &fakeProbe{}
		b := &fakeProbe{}
		a.paused.Store(true)
		b.paused.Store(true)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cancelled := make(chan struct{})
		idleTimeout := 10 * time.Second
		w := NewSessionIdleWatcher([]IdleProbe{a, b}, func() { close(cancelled) }, idleTimeout)
		go w.Run(ctx)

		select {
		case <-cancelled:
		case <-time.After(idleTimeout + time.Minute):
			t.Fatal("watcher did not cancel after all probes stayed paused")
		}
	})
}

// TestSessionIdleWatcherCancelsWhenRelayIdleButUnpaused is the focused
// regression for the reported "SESSION_IDLE_TIMEOUT does not stop the session"
// bug. The relay mixer is never paused on silence (its pause state is
// presence-driven by the router, and it has no DrainWatcher), so it stays
// unpaused while callers sit in the channel but say nothing. The watcher must
// still auto-stop, because the relay mixer reports a long IdleFor (no audio).
func TestSessionIdleWatcherCancelsWhenRelayIdleButUnpaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chanMixer := &fakeProbe{}
		chanMixer.paused.Store(true) // DrainWatcher paused it after silence
		relay := &fakeProbe{}        // never paused (no DrainWatcher attached)
		relay.idleNanos.Store(int64(time.Hour))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cancelled := make(chan struct{})
		idleTimeout := 10 * time.Second
		go NewSessionIdleWatcher([]IdleProbe{chanMixer, relay}, func() { close(cancelled) }, idleTimeout).Run(ctx)

		select {
		case <-cancelled:
		case <-time.After(idleTimeout + time.Minute):
			t.Fatal("watcher did not cancel though every mixer was quiet (idle relay)")
		}
	})
}

// TestSessionIdleWatcherCancelLatency pins down how long the watcher takes to
// cancel: quietSince is recorded on the FIRST all-quiet tick (one interval in),
// then a full idleTimeout must elapse — so the real latency is interval+idleTimeout.
// Only observable with a fake clock.
func TestSessionIdleWatcherCancelLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &fakeProbe{}
		a.paused.Store(true)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cancelled := make(chan struct{})
		idleTimeout := 10 * time.Second // interval clamps to idleTimeout/10 = 1s
		start := time.Now()
		go NewSessionIdleWatcher([]IdleProbe{a}, func() { close(cancelled) }, idleTimeout).Run(ctx)

		<-cancelled
		elapsed := time.Since(start)
		// First all-quiet observation lands at ~1 interval (1s); cancel fires one
		// idleTimeout later. Allow one extra interval of slack.
		if elapsed < idleTimeout || elapsed > idleTimeout+2*time.Second {
			t.Fatalf("cancel latency = %v, want within [%v, %v]", elapsed, idleTimeout, idleTimeout+2*time.Second)
		}
	})
}

// TestSessionIdleWatcherNoCancelWhenRelayActive checks that an unpaused mixer
// that is still consuming audio (IdleFor ~ 0) keeps the session alive even
// though the channel mixers are paused — audio is actively flowing to guests.
func TestSessionIdleWatcherNoCancelWhenRelayActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chanMixer := &fakeProbe{}
		chanMixer.paused.Store(true)
		relay := &fakeProbe{} // unpaused, idleNanos == 0 → actively relaying

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cancelled := make(chan struct{})
		idleTimeout := 10 * time.Second
		go NewSessionIdleWatcher([]IdleProbe{chanMixer, relay}, func() { close(cancelled) }, idleTimeout).Run(ctx)

		// Advance well past several idle windows; the active relay must keep it alive.
		time.Sleep(5 * idleTimeout)
		synctest.Wait()
		select {
		case <-cancelled:
			t.Fatal("watcher cancelled despite an actively relaying mixer")
		default:
		}

		cancel()
		synctest.Wait()
	})
}

// TestSessionIdleWatcherDisabled checks that idleTimeout <= 0 makes Run a no-op.
func TestSessionIdleWatcherDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &fakeProbe{}
		a.paused.Store(true)

		cancelled := make(chan struct{})
		done := make(chan struct{})
		w := NewSessionIdleWatcher([]IdleProbe{a}, func() { close(cancelled) }, 0)
		go func() {
			w.Run(context.Background())
			close(done)
		}()

		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return immediately when idleTimeout <= 0")
		}
		select {
		case <-cancelled:
			t.Fatal("cancelFunc invoked when watcher disabled")
		default:
		}
	})
}

// TestSessionIdleWatcherEmpty checks that an empty mixers slice makes Run a no-op.
func TestSessionIdleWatcherEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan struct{})
		done := make(chan struct{})
		w := NewSessionIdleWatcher(nil, func() { close(cancelled) }, time.Second)
		go func() {
			w.Run(context.Background())
			close(done)
		}()

		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return immediately when no probes")
		}
		select {
		case <-cancelled:
			t.Fatal("cancelFunc invoked with no probes")
		default:
		}
	})
}

// TestSessionIdleWatcherResetsOnUnpause checks that quietSince resets when a
// probe transitions back to active for at least idleUnpauseHysteresis ticks —
// i.e. activity bumps the watcher window.
func TestSessionIdleWatcherResetsOnUnpause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &fakeProbe{}
		a.paused.Store(true)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cancelled := make(chan struct{})
		idleTimeout := 10 * time.Second // interval = 1s
		go NewSessionIdleWatcher([]IdleProbe{a}, func() { close(cancelled) }, idleTimeout).Run(ctx)

		// Accumulate most of the window while paused (quietSince set ~1s in).
		time.Sleep(6 * time.Second)
		synctest.Wait()

		// Become active long enough to clear hysteresis (>= 2 ticks) and reset
		// quietSince. idleNanos stays 0 so the probe is genuinely active, not quiet.
		a.paused.Store(false)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		a.paused.Store(true)

		// Only ~6s have elapsed since the reset (< idleTimeout), even though the
		// original window would long since have fired — so cancel must NOT have run.
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case <-cancelled:
			t.Fatal("watcher cancelled despite mid-window activity resetting quietSince")
		default:
		}

		cancel()
		synctest.Wait()
	})
}
