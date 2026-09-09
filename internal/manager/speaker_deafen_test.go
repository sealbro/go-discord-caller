package manager

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

const (
	testGuild   = snowflake.ID(1)
	testBot     = snowflake.ID(100)
	testOther   = snowflake.ID(101)
	testDelay   = 40 * time.Millisecond
	settleAfter = 10 * testDelay
)

// deafRecorder captures every state change the controller drives.
type deafRecorder struct {
	mu       sync.Mutex
	events   []deafEvent
	err      error
	nAttempt int
	// block, when non-nil, holds setDeaf until closed so a test can observe
	// the window while a change is in flight.
	block chan struct{}
}

type deafEvent struct {
	botID snowflake.ID
	deaf  bool
}

func (r *deafRecorder) set(_ context.Context, _, botID snowflake.ID, deaf bool) error {
	r.mu.Lock()
	r.nAttempt++
	err := r.err
	block := r.block
	r.mu.Unlock()

	if block != nil {
		<-block
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, deafEvent{botID, deaf})
	return nil
}

// attempts counts calls, including ones that returned an error.
func (r *deafRecorder) attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nAttempt
}

func (r *deafRecorder) snapshot() []deafEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deafEvent(nil), r.events...)
}

// waitFor polls until cond holds or the deadline passes. The controller applies
// changes on their own goroutines, so assertions must be eventual.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestController(rec *deafRecorder) *deafController {
	return &deafController{
		guildID: testGuild,
		setDeaf: rec.set,
		delay:   testDelay,
		members: map[snowflake.ID]*deafMember{},
	}
}

func lastEvent(rec *deafRecorder) (deafEvent, bool) {
	ev := rec.snapshot()
	if len(ev) == 0 {
		return deafEvent{}, false
	}
	return ev[len(ev)-1], true
}

// TestDeafControllerDeafensIdleSourceAfterDelay is case 2/4: a capture bot whose
// channel holds nobody with the role must end up deafened — but only after the
// idle delay, never instantly.
func TestDeafControllerDeafensIdleSourceAfterDelay(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, false) // capture bot starts hearing

	c.Observe(map[snowflake.ID]bool{testBot: false})

	// Must not have fired yet: deafening is deliberately delayed.
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("deafened immediately (%v); the delay is what suppresses flapping", got)
	}

	waitFor(t, "delayed deafen", func() bool {
		ev, ok := lastEvent(rec)
		return ok && ev == deafEvent{testBot, true}
	})
}

// TestDeafControllerUndeafensImmediately is cases 5/6: the moment a source goes
// live again the bot must be undeafened without waiting, because the delay
// would cost the newly-permitted speaker their first words.
func TestDeafControllerUndeafensImmediately(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	// A long delay: if undeafening ever went through the timer path, the
	// assertion below would time out long before it fired. Asserting only
	// "eventually undeafened" would pass either way and prove nothing.
	c.delay = 30 * time.Second
	c.Register(testBot, true) // starts deafened

	c.Observe(map[snowflake.ID]bool{testBot: true})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ev, ok := lastEvent(rec); ok && ev == (deafEvent{testBot, false}) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("undeafen did not happen promptly; it must bypass the deafen delay " +
		"or the newly-permitted speaker loses their first words")
}

// TestDeafControllerCancelsPendingDeafen is the flapping case: a caller who
// leaves and returns inside the delay window must produce no change at all, and
// therefore no audit-log entry.
func TestDeafControllerCancelsPendingDeafen(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, false)

	c.Observe(map[snowflake.ID]bool{testBot: false}) // arms the deafen timer
	c.Observe(map[snowflake.ID]bool{testBot: true})  // caller returns in time

	time.Sleep(settleAfter)

	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("flapping produced %v; want no changes at all", got)
	}
}

// TestDeafControllerIgnoresRedundantState guards against audit-log spam: a
// steady stream of recomputes reporting the state the bot is already in must
// issue nothing.
func TestDeafControllerIgnoresRedundantState(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, false)

	for range 5 {
		c.Observe(map[snowflake.ID]bool{testBot: true})
	}
	time.Sleep(settleAfter)

	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("redundant observations produced %v; want none", got)
	}
}

// TestDeafControllerDeafensUnknownSources covers bots the router never reports:
// non-capture speakers, and the second and later speakers sharing a channel
// whose FanoutHandles are never installed. Absent from the map means not a live
// source, which means deafen.
func TestDeafControllerDeafensUnknownSources(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testOther, false) // registered, but never a router source

	c.Observe(map[snowflake.ID]bool{testBot: true}) // only the other bot is live

	waitFor(t, "deafen of non-source bot", func() bool {
		ev, ok := lastEvent(rec)
		return ok && ev == deafEvent{testOther, true}
	})
}

// TestDeafControllerCloseUndeafens is the teardown guarantee: the flag persists
// on the guild member, so a session must never end leaving one set.
func TestDeafControllerCloseUndeafens(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, true) // controller believes it is deafened

	c.Close(context.Background())

	found := false
	for _, ev := range rec.snapshot() {
		if ev == (deafEvent{testBot, false}) {
			found = true
		}
	}
	if !found {
		t.Errorf("Close did not undeafen; events = %v", rec.snapshot())
	}
}

// TestDeafControllerCloseCancelsPendingDeafen stops a timer armed just before
// teardown from firing afterwards and re-deafening a bot that has left.
func TestDeafControllerCloseCancelsPendingDeafen(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, false)

	c.Observe(map[snowflake.ID]bool{testBot: false}) // arm
	c.Close(context.Background())
	time.Sleep(settleAfter)

	for _, ev := range rec.snapshot() {
		if ev.deaf {
			t.Errorf("a deafen fired after Close; events = %v", rec.snapshot())
		}
	}
}

// TestDeafControllerObserveAfterCloseIsInert protects against a late router
// recompute racing teardown.
func TestDeafControllerObserveAfterCloseIsInert(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, false)
	c.Close(context.Background())

	c.Observe(map[snowflake.ID]bool{testBot: false})
	time.Sleep(settleAfter)

	for _, ev := range rec.snapshot() {
		if ev.deaf {
			t.Errorf("Observe after Close still deafened; events = %v", rec.snapshot())
		}
	}
}

// permError builds the error Discord returns when the bot may not deafen.
func permError() error {
	return &rest.Error{Code: jsonErrMissingPermissions, Message: "Missing Permissions"}
}

// TestDeafControllerLatchesOffOnPermissionDenied is the fix for a real defect
// found in an integration run: without the latch, a guild that has not granted
// DEAFEN_MEMBERS gets a fresh 403 for every bot every deafenDelay for the whole
// session. Discord's edge treats sustained 4xx as abuse, so this is not merely
// wasteful.
func TestDeafControllerLatchesOffOnPermissionDenied(t *testing.T) {
	rec := &deafRecorder{err: permError()}
	c := newTestController(rec)
	c.Register(testBot, false)

	c.Observe(map[snowflake.ID]bool{testBot: false})
	waitFor(t, "first (failing) attempt", func() bool { return len(rec.snapshot()) == 0 && c.isDisabled() })

	// Any number of further recomputes must produce no further attempts.
	for range 5 {
		c.Observe(map[snowflake.ID]bool{testBot: false})
	}
	time.Sleep(settleAfter)

	if n := rec.attempts(); n != 1 {
		t.Errorf("made %d attempts after a permission denial, want exactly 1", n)
	}
}

// TestDeafControllerDoesNotRecordFailedChange covers the second defect: state
// was recorded before Discord accepted it, so a failed deafen looked applied
// and Close would then issue a pointless undeafen for it.
func TestDeafControllerDoesNotRecordFailedChange(t *testing.T) {
	rec := &deafRecorder{err: permError()}
	c := newTestController(rec)
	c.Register(testBot, false)

	c.Observe(map[snowflake.ID]bool{testBot: false})
	waitFor(t, "failed deafen attempt", func() bool { return rec.attempts() == 1 })

	c.mu.Lock()
	deaf := c.members[testBot].deaf
	c.mu.Unlock()
	if deaf {
		t.Error("a rejected deafen was recorded as applied")
	}
}

// TestDeafControllerNoDuplicateInFlight pins the in-flight guard: recomputes
// arriving while a change is being applied must not queue a second PATCH for
// the same bot.
func TestDeafControllerNoDuplicateInFlight(t *testing.T) {
	rec := &deafRecorder{block: make(chan struct{})}
	c := newTestController(rec)
	c.Register(testBot, true)

	c.Observe(map[snowflake.ID]bool{testBot: true}) // undeafen, will block in setDeaf
	waitFor(t, "apply to start", func() bool { return rec.attempts() == 1 })

	for range 5 {
		c.Observe(map[snowflake.ID]bool{testBot: true})
	}
	close(rec.block)
	time.Sleep(settleAfter)

	if n := rec.attempts(); n != 1 {
		t.Errorf("issued %d changes for one transition, want 1", n)
	}
}
