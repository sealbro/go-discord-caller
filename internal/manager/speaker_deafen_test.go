package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry/spantest"
	"go.opentelemetry.io/otel/codes"
)

const (
	testGuild = snowflake.ID(1)
	testBot   = snowflake.ID(100)
	testOther = snowflake.ID(101)
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

// setErr swaps the error setDeaf returns, so a test can make a change fail
// once and then succeed.
func (r *deafRecorder) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func newTestController(rec *deafRecorder) *deafController {
	return &deafController{
		guildID: testGuild,
		setDeaf: rec.set,
		delay:   deafenDelay,
		members: map[snowflake.ID]*deafMember{},
	}
}

// settle advances the bubble's clock by d and lets every goroutine the
// controller started run until it blocks.
func settle(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func assertAttempts(t *testing.T, rec *deafRecorder, want int, when string) {
	t.Helper()
	if got := rec.attempts(); got != want {
		t.Fatalf("%s: %d attempts, want %d", when, got, want)
	}
}

func assertLastEvent(t *testing.T, rec *deafRecorder, want deafEvent, when string) {
	t.Helper()
	ev := rec.snapshot()
	if len(ev) == 0 || ev[len(ev)-1] != want {
		t.Fatalf("%s: changes = %v, want the last to be %v", when, ev, want)
	}
}

// TestDeafControllerDeafensIdleSourceAfterDelay is case 2/4: a capture bot whose
// channel holds nobody with the role must end up deafened — but only after the
// idle delay, never instantly.
func TestDeafControllerDeafensIdleSourceAfterDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, false) // capture bot starts hearing

		c.Observe(map[snowflake.ID]bool{testBot: false})

		settle(deafenDelay - time.Nanosecond)
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("deafened before the delay ran out (%v); the delay is what suppresses flapping", got)
		}

		settle(time.Nanosecond)
		assertLastEvent(t, rec, deafEvent{testBot, true}, "once the delay ran out")
	})
}

// TestDeafControllerUndeafensImmediately is cases 5/6: the moment a source goes
// live again the bot must be undeafened without waiting, because the delay
// would cost the newly-permitted speaker their first words.
func TestDeafControllerUndeafensImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, true) // starts deafened

		c.Observe(map[snowflake.ID]bool{testBot: true})

		// The bubble's clock has not moved, so a change routed through the
		// timer has not happened yet.
		synctest.Wait()
		if ev := rec.snapshot(); len(ev) != 1 || ev[0] != (deafEvent{testBot, false}) {
			t.Fatalf("changes = %v before any time passed; undeafen must bypass the deafen delay "+
				"or the newly-permitted speaker loses their first words", ev)
		}
	})
}

// TestDeafControllerCancelsPendingDeafen is the flapping case: a caller who
// leaves and returns inside the delay window must produce no change at all, and
// therefore no audit-log entry.
func TestDeafControllerCancelsPendingDeafen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, false)

		c.Observe(map[snowflake.ID]bool{testBot: false}) // arms the deafen timer
		settle(deafenDelay / 2)
		c.Observe(map[snowflake.ID]bool{testBot: true}) // caller returns in time

		settle(2 * deafenDelay)
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("flapping produced %v; want no changes at all", got)
		}
	})
}

// TestDeafControllerIgnoresRedundantState guards against audit-log spam: a
// steady stream of recomputes reporting the state the bot is already in must
// issue nothing.
func TestDeafControllerIgnoresRedundantState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, false)

		for range 5 {
			c.Observe(map[snowflake.ID]bool{testBot: true})
		}
		settle(2 * deafenDelay)

		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("redundant observations produced %v; want none", got)
		}
	})
}

// TestDeafControllerDeafensUnknownSources covers bots the router never reports:
// non-capture speakers, and the second and later speakers sharing a channel
// whose FanoutHandles are never installed. Absent from the map means not a live
// source, which means deafen.
func TestDeafControllerDeafensUnknownSources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testOther, false) // registered, but never a router source

		c.Observe(map[snowflake.ID]bool{testBot: true}) // only the other bot is live

		settle(deafenDelay)
		assertLastEvent(t, rec, deafEvent{testOther, true}, "after the delay")
	})
}

// TestDeafControllerCloseUndeafens is the teardown guarantee: the flag persists
// on the guild member, so a session must never end leaving one set.
func TestDeafControllerCloseUndeafens(t *testing.T) {
	rec := &deafRecorder{}
	c := newTestController(rec)
	c.Register(testBot, true) // controller believes it is deafened

	c.Close(context.Background())

	if got := rec.snapshot(); len(got) != 1 || got[0] != (deafEvent{testBot, false}) {
		t.Errorf("Close did not undeafen; events = %v", got)
	}
}

// TestDeafControllerCloseCancelsPendingDeafen stops a timer armed just before
// teardown from firing afterwards and re-deafening a bot that has left.
func TestDeafControllerCloseCancelsPendingDeafen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, false)

		c.Observe(map[snowflake.ID]bool{testBot: false}) // arm
		c.Close(context.Background())
		settle(2 * deafenDelay)

		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("a change fired after Close; events = %v", got)
		}
	})
}

// TestDeafControllerObserveAfterCloseIsInert protects against a late router
// recompute racing teardown.
func TestDeafControllerObserveAfterCloseIsInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{}
		c := newTestController(rec)
		c.Register(testBot, false)
		c.Close(context.Background())

		c.Observe(map[snowflake.ID]bool{testBot: false})
		settle(2 * deafenDelay)

		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("Observe after Close still changed state; events = %v", got)
		}
	})
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
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{err: permError()}
		c := newTestController(rec)
		c.Register(testBot, false)

		c.Observe(map[snowflake.ID]bool{testBot: false})
		settle(deafenDelay)
		assertAttempts(t, rec, 1, "after the delay")
		if !c.isDisabled() {
			t.Fatal("a permission denial did not latch the controller off")
		}

		// Any number of further recomputes must produce no further attempts.
		for range 5 {
			c.Observe(map[snowflake.ID]bool{testBot: false})
		}
		settle(10 * deafenDelay)
		assertAttempts(t, rec, 1, "after a permission denial")
	})
}

// TestDeafControllerDoesNotRecordFailedChange covers the second defect: state
// was recorded before Discord accepted it, so a failed deafen looked applied
// and Close would then issue a pointless undeafen for it.
func TestDeafControllerDoesNotRecordFailedChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{err: permError()}
		c := newTestController(rec)
		c.Register(testBot, false)

		c.Observe(map[snowflake.ID]bool{testBot: false})
		settle(deafenDelay)
		assertAttempts(t, rec, 1, "after the delay")

		c.mu.Lock()
		deaf := c.members[testBot].deaf
		c.mu.Unlock()
		if deaf {
			t.Error("a rejected deafen was recorded as applied")
		}
	})
}

// TestDeafControllerNoDuplicateInFlight pins the in-flight guard: recomputes
// arriving while a change is being applied must not queue a second PATCH for
// the same bot.
func TestDeafControllerNoDuplicateInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{block: make(chan struct{})}
		c := newTestController(rec)
		c.Register(testBot, true)

		c.Observe(map[snowflake.ID]bool{testBot: true}) // undeafen, will block in setDeaf
		synctest.Wait()
		assertAttempts(t, rec, 1, "with the undeafen in flight")

		for range 5 {
			c.Observe(map[snowflake.ID]bool{testBot: true})
		}
		close(rec.block)
		settle(2 * deafenDelay)
		assertAttempts(t, rec, 1, "for one transition")
	})
}

// TestDeafenReadinessOK pins the predicate the /start reply branches on. The
// hierarchy half matters as much as the permission half: Discord answers 50013
// for either, so a guild that granted DEAFEN_MEMBERS but left the owner bot
// below its speakers still gets nothing, and must still be told.
func TestDeafenReadinessOK(t *testing.T) {
	cases := []struct {
		name string
		in   DeafenReadiness
		want bool
	}{
		{"all good", DeafenReadiness{}, true},
		{"no permission", DeafenReadiness{MissingPermission: true}, false},
		{"outranked by a speaker", DeafenReadiness{OutrankedBy: []snowflake.ID{1}}, false},
		{"both problems", DeafenReadiness{MissingPermission: true, OutrankedBy: []snowflake.ID{1}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.OK(); got != tc.want {
				t.Errorf("OK() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeafControllerCloseWaitsForInFlightDeafen covers the teardown race raised
// in review of PR #63.
//
// A deafen whose timer fires moments before teardown has not recorded itself
// yet, so a Close that snapshotted immediately would see deaf=false, skip the
// bot, and let the PATCH land afterwards — leaving the member server-deafened
// with nothing left to repair it. For the owner bot that is permanent: it is
// not covered by the per-speaker Undeafen in BuildSpeakerCleanup, so it would
// capture silence in every later raid.
func TestDeafControllerCloseWaitsForInFlightDeafen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{block: make(chan struct{})}
		c := newTestController(rec)
		c.Register(testBot, false)

		c.Observe(map[snowflake.ID]bool{testBot: false}) // arm the deafen
		settle(deafenDelay)
		assertAttempts(t, rec, 1, "with the deafen in flight")

		closed := make(chan struct{})
		go func() {
			c.Close(context.Background())
			close(closed)
		}()

		// Every goroutine is blocked now: Close has run as far as it can while
		// the deafen is still in flight.
		synctest.Wait()
		select {
		case <-closed:
			t.Error("Close returned while a deafen was still in flight; it did not wait")
		default:
		}

		close(rec.block) // let the in-flight deafen land
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("Close did not return once the in-flight deafen landed")
		}

		want := []deafEvent{{testBot, true}, {testBot, false}}
		if got := rec.snapshot(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("changes = %v, want %v: Close must undo the deafen that landed while it waited", got, want)
		}
	})
}

// TestDeafControllerReconcilesStateObservedDuringApply reproduces a session
// going permanently silent after a deafen.
//
// A caller arriving while a deafen PATCH is still in flight is dropped on the
// floor: Observe skips every member with inFlight set, and apply never
// re-reads the desired state once Discord answers. Recomputes are purely
// event-driven, so nothing revisits the decision — the bot stays deafened with
// a caller in its channel and captures silence for the rest of the raid.
func TestDeafControllerReconcilesStateObservedDuringApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{block: make(chan struct{})}
		c := newTestController(rec)
		c.Register(testBot, false) // capture bot starts hearing

		c.Observe(map[snowflake.ID]bool{testBot: false}) // channel empties: arm deafen
		settle(deafenDelay)
		assertAttempts(t, rec, 1, "with the deafen in flight")

		// A caller arrives while the deafen PATCH is in flight.
		c.Observe(map[snowflake.ID]bool{testBot: true})
		close(rec.block)

		synctest.Wait()
		assertLastEvent(t, rec, deafEvent{testBot, false}, "once the deafen landed with a caller present")
	})
}

// TestDeafControllerRetriesRejectedChange covers the non-permission half of the
// same defect. A change Discord rejects for a transient reason left the member
// at the wrong state with nothing to revisit it, because recomputes only happen
// on voice events. Retries back off by the delay rather than spinning.
func TestDeafControllerRetriesRejectedChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{err: errors.New("503 service unavailable")}
		c := newTestController(rec)
		c.Register(testBot, true) // deafened; the router wants it hearing

		c.Observe(map[snowflake.ID]bool{testBot: true})
		synctest.Wait()
		assertAttempts(t, rec, 1, "after the immediate undeafen")

		rec.setErr(nil)
		settle(deafenDelay - time.Nanosecond)
		assertAttempts(t, rec, 1, "before the retry delay ran out")
		settle(time.Nanosecond)
		assertLastEvent(t, rec, deafEvent{testBot, false}, "once the retry delay ran out")
	})
}

// TestDeafControllerStopsRetryingRejectedChange bounds those retries. A
// rejection the permission latch does not cover — an unknown member, a bot no
// longer connected to voice — persists for the session, and retrying it every
// delay until teardown is the behaviour the latch exists to prevent.
func TestDeafControllerStopsRetryingRejectedChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{err: errors.New("10007 unknown member")}
		c := newTestController(rec)
		c.Register(testBot, true)

		c.Observe(map[snowflake.ID]bool{testBot: true})
		settle((deafRetryLimit - 1) * deafenDelay)
		assertAttempts(t, rec, deafRetryLimit, "once the retries ran")

		settle(10 * deafenDelay)
		assertAttempts(t, rec, deafRetryLimit, "long after the last retry (retries are not bounded)")

		// A fresh recompute restores the budget: the router is still asking, so
		// this is not the quiet chain the limit exists to stop.
		c.Observe(map[snowflake.ID]bool{testBot: true})
		settle(deafenDelay)
		assertAttempts(t, rec, deafRetryLimit+1, "after a recompute")
	})
}

// A change Discord keeps rejecting for a reason retrying cannot fix — 10007
// unknown member, or the bot no longer being in voice — is still a retry when
// the router re-asks for it. The router recomputes on every voice join, leave
// and role change, so taking the immediate path on each one turns a busy
// channel into a burst of rejected moderation requests; the delay is what
// paces them.
func TestRecomputesDoNotReissueARejectedChangeUnpaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deafRecorder{err: errors.New("unknown member")}
		c := newTestController(rec)
		c.Register(testBot, true) // deafened, and the router wants it hearing

		capturing := map[snowflake.ID]bool{testBot: true}
		c.Observe(capturing)
		synctest.Wait()
		assertAttempts(t, rec, 1, "after the immediate undeafen")

		// None of these recomputes is a new request: the desired state is the one
		// Discord just refused.
		for range 10 {
			c.Observe(capturing)
			synctest.Wait()
		}
		settle(deafenDelay - time.Nanosecond)
		if extra := rec.attempts() - 1; extra > 0 {
			t.Errorf("%d extra PATCHes inside the retry delay, from 10 recomputes; "+
				"re-asking for a rejected change must wait out the delay like any other retry", extra)
		}

		settle(time.Nanosecond)
		assertAttempts(t, rec, 2, "once the retry delay ran out")
	})
}

type lateApplyRecorder struct {
	closed atomic.Bool
	late   atomic.Int32
}

func (r *lateApplyRecorder) set(context.Context, snowflake.ID, snowflake.ID, bool) error {
	if r.closed.Load() {
		r.late.Add(1)
	}
	return nil
}

// Observe admits an undeafen under c.mu and starts it once unlocked. Close must
// count the change from the moment it is admitted, or it can return before the
// change has even started. That window has no blocking point a test could hold
// it open at, so this runs the two against each other until one lands in it.
func TestDeafControllerCloseWaitsForChangeBeingStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &lateApplyRecorder{}
		for range 3000 {
			rec.closed.Store(false)
			c := &deafController{guildID: testGuild, setDeaf: rec.set, delay: deafenDelay, members: map[snowflake.ID]*deafMember{}}
			c.Register(testBot, true)
			var wg sync.WaitGroup
			wg.Go(func() { c.Observe(map[snowflake.ID]bool{testBot: true}) })
			wg.Go(func() {
				c.Close(context.Background())
				rec.closed.Store(true)
			})
			wg.Wait()
			synctest.Wait()
		}
		if n := rec.late.Load(); n != 0 {
			t.Errorf("%d deaf change(s) started after Close returned: Close did not wait for a change it had already admitted", n)
		}
	})
}

// stalledFirstRecorder answers its first deaf change only when the request's
// context ends, as a PATCH Discord never answers would, and fails every later
// call made on an expired context, as disgo does.
type stalledFirstRecorder struct {
	mu         sync.Mutex
	calls      int
	undeafened []snowflake.ID
}

func (r *stalledFirstRecorder) set(ctx context.Context, _, botID snowflake.ID, deaf bool) error {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()

	if first {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Hour):
			return errors.New("stalled request was never given a deadline")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !deaf {
		r.undeafened = append(r.undeafened, botID)
	}
	return nil
}

// One unanswered undeafen at teardown must not cost every other bot its
// undeafen. Production, 2026-11-26: six bots failed in the same second, five
// seconds after the raid stopped, because the first request spent the whole
// teardown budget and the rest were sent on an expired context.
func TestTeardownUndeafensEveryBotPastAStalledRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stalledFirstRecorder{}
		c := &deafController{guildID: testGuild, setDeaf: rec.set, delay: deafenDelay, members: map[snowflake.ID]*deafMember{}}
		bots := []snowflake.ID{701, 702, 703, 704, 705, 706}
		for _, id := range bots {
			c.Register(id, true)
		}

		left := false
		undeafenThenLeave(c, func() { left = true })()

		if !left {
			t.Fatal("speakers never left voice")
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if got, want := len(rec.undeafened), len(bots)-1; got != want {
			t.Errorf("%d of %d bots undeafened behind one stalled request (undeafened %v); "+
				"a slow request must not fail the bots queued after it", got, want, rec.undeafened)
		}
	})
}

// Teardown undeafens carry no raid context, so without their own span a slow
// one cannot be told apart from a rate-limit wait. Each must leave a span with
// its bot, and the stalled one must carry its error.
func TestTeardownUndeafenLeavesASpanPerBot(t *testing.T) {
	ended := spantest.Record()
	synctest.Test(t, func(t *testing.T) {
		calls := &stalledFirstRecorder{}
		c := &deafController{guildID: testGuild, setDeaf: calls.set, delay: deafenDelay, members: map[snowflake.ID]*deafMember{}}
		bots := map[string]bool{"711": true, "712": true, "713": true}
		for id := range bots {
			sf, _ := snowflake.Parse(id)
			c.Register(sf, true)
		}

		undeafenThenLeave(c, func() {})()

		failed := 0
		seen := map[string]bool{}
		for _, s := range ended() {
			if s.Name() != "voice.deaf.undeafen" {
				continue
			}
			for _, a := range s.Attributes() {
				if a.Key == "bot.id" && bots[a.Value.AsString()] {
					seen[a.Value.AsString()] = true
					if s.Status().Code == codes.Error {
						failed++
					}
				}
			}
		}
		if len(seen) != len(bots) {
			t.Errorf("undeafen spans for bots %v, want one for each of %v", seen, bots)
		}
		if failed != 1 {
			t.Errorf("%d undeafen spans marked failed, want 1: the stalled request's error must be on its span", failed)
		}
	})
}
