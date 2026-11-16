package dave

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/godave"
)

type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func newClockedSession(t *testing.T) (godave.Session, *fakeSession, *manualClock) {
	t.Helper()
	backend := &fakeSession{known: make(map[godave.UserID]struct{})}
	clock := &manualClock{at: time.Unix(1_700_000_000, 0)}
	inner := func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return backend }
	return Instrument(inner, NewStats(), withClock(clock.now))(slog.Default(), "bot-1", nil), backend, clock
}

func (f *fakeSession) readyAsked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyCalls
}

// disgo's audio sender and receiver each ask Ready on every 20 ms tick, and
// golibdave answers with two calls into libdave: 100 per second for every bot
// in voice, to learn something that changes a few times per session.
func TestReadyIsNotAskedOfTheBackendOnEveryTick(t *testing.T) {
	session, backend, clock := newClockedSession(t)
	backend.ready = true

	const ticks = 100
	for range ticks {
		if !session.Ready() {
			t.Fatal("Ready = false, want the backend's true")
		}
		clock.advance(readyTTL / ticks)
	}

	if got := backend.readyAsked(); got > 2 {
		t.Errorf("backend asked %d times for %d ticks inside one %v interval, want it remembered", got, ticks, readyTTL)
	}
}

// A sender that still believes the session is not ready discards the speech
// queued for it, so the answer has to follow a transition without waiting.
func TestReadyFollowsATransitionAtOnce(t *testing.T) {
	for name, transition := range map[string]func(godave.Session){
		"select protocol ack": func(s godave.Session) { s.OnSelectProtocolAck(1) },
		"prepare transition":  func(s godave.Session) { s.OnDavePrepareTransition(0, 1) },
		"execute transition":  func(s godave.Session) { s.OnDaveExecuteTransition(1) },
		"commit transition":   func(s godave.Session) { s.OnDaveMLSPrepareCommitTransition(1, nil) },
		"welcome":             func(s godave.Session) { s.OnDaveMLSWelcome(1, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			session, backend, _ := newClockedSession(t)
			if session.Ready() {
				t.Fatal("Ready = true before any transition")
			}

			backend.ready = true
			transition(session)

			if !session.Ready() {
				t.Error("Ready still false after the transition that made the session ready")
			}
		})
	}
}

// The handlers above are the only places golibdave v0.3.0 touches the
// encryptor. Should that stop being true, the answer may be stale, but not for
// longer than readyTTL.
func TestReadyIsAskedAgainAfterTheInterval(t *testing.T) {
	session, backend, clock := newClockedSession(t)
	if session.Ready() {
		t.Fatal("Ready = true before any transition")
	}

	backend.ready = true
	clock.advance(readyTTL)

	if !session.Ready() {
		t.Errorf("Ready still false %v after the backend became ready", readyTTL)
	}
}
