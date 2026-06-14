package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// callerCount is a CallerEnumerator that returns canned values per channel
// and counts how many times EnumerateCallers has been invoked. When users is
// nil it synthesises non-zero placeholder userIDs derived from channelID, so
// existing tests that only set counts continue to work without specifying
// per-user identities.
type callerCount struct {
	counts map[snowflake.ID]int
	users  map[snowflake.ID][]snowflake.ID
	calls  atomic.Int32
}

func (c *callerCount) EnumerateCallers(channelID, _ snowflake.ID) []snowflake.ID {
	c.calls.Add(1)
	if u, ok := c.users[channelID]; ok {
		out := make([]snowflake.ID, len(u))
		copy(out, u)
		return out
	}
	n := c.counts[channelID]
	out := make([]snowflake.ID, n)
	for i := range out {
		// Synth user IDs that are stable per (channel, index) but non-zero
		// and unlikely to collide with explicit fixtures.
		out[i] = snowflake.ID(uint64(channelID)*1000 + uint64(i+1))
	}
	return out
}

// HasListeners defaults to true so cascade-focused tests don't need to wire
// listener fixtures. Per-test overrides can be added once the field exists.
func (c *callerCount) HasListeners(_ snowflake.ID) bool { return true }

func chID(i uint64) snowflake.ID { return snowflake.ID(i) }

func TestComputeRoutes_NPlus3MixMinusAlwaysRunsMixer(t *testing.T) {
	// With N≥3 mix-minus, every destination is fed by ≥2 sources. Even when
	// every channel has C=1, the mixer is mandatory — otherwise multiple
	// copy-mode sources would race on the same destination chOuts. This is
	// the §1 row-4 rule ("≥2 sources feeding D → mixer runs; sources forced
	// to mix").
	sources := []routeSource{
		{id: chID(1), channelID: chID(1)},
		{id: chID(2), channelID: chID(2)},
		{id: chID(3), channelID: chID(3)},
	}
	dests := []routeDest{
		{channelID: chID(1), sourceIDs: []snowflake.ID{chID(2), chID(3)}},
		{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1), chID(3)}},
		{channelID: chID(3), sourceIDs: []snowflake.ID{chID(1), chID(2)}},
	}
	counts := map[snowflake.ID]int{chID(1): 1, chID(2): 1, chID(3): 1}

	sourceModes, destMix := computeRoutes(sources, dests, counts)

	for _, s := range sources {
		if sourceModes[s.id] != routeMix {
			t.Errorf("N=3 mix-minus, all C=1: source %s must be routeMix (multi-source dest race); got %v", s.id, sourceModes[s.id])
		}
	}
	for _, d := range dests {
		if !destMix[d.channelID] {
			t.Errorf("N=3 mix-minus: dest %s must be marked mix; was not", d.channelID)
		}
	}
}

func TestComputeRoutes_N2MixMinusAllCopyWhenBothC1(t *testing.T) {
	// N=2 mix-minus is the boundary where copy actually pays off: each dest
	// has exactly one feeding source, so no chOuts race is possible.
	sources := []routeSource{
		{id: chID(1), channelID: chID(1)},
		{id: chID(2), channelID: chID(2)},
	}
	dests := []routeDest{
		{channelID: chID(1), sourceIDs: []snowflake.ID{chID(2)}},
		{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}},
	}
	counts := map[snowflake.ID]int{chID(1): 1, chID(2): 1}

	sourceModes, destMix := computeRoutes(sources, dests, counts)

	for _, s := range sources {
		if sourceModes[s.id] != routeCopy {
			t.Errorf("N=2 mix-minus, all C=1: source %s must be routeCopy; got %v", s.id, sourceModes[s.id])
		}
	}
	for _, d := range dests {
		if destMix[d.channelID] {
			t.Errorf("N=2 mix-minus, all C=1: dest %s must NOT be marked mix", d.channelID)
		}
	}
}

func TestComputeRoutes_OffWhenChannelEmpty(t *testing.T) {
	sources := []routeSource{
		{id: chID(1), channelID: chID(1)},
		{id: chID(2), channelID: chID(2)},
	}
	dests := []routeDest{
		{channelID: chID(1), sourceIDs: []snowflake.ID{chID(2)}},
		{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}},
	}
	counts := map[snowflake.ID]int{chID(1): 0, chID(2): 1}

	sourceModes, _ := computeRoutes(sources, dests, counts)

	if sourceModes[chID(1)] != routeOff {
		t.Errorf("empty channel: want routeOff, got %v", sourceModes[chID(1)])
	}
	if sourceModes[chID(2)] != routeCopy {
		t.Errorf("solo channel: want routeCopy, got %v", sourceModes[chID(2)])
	}
}

func TestComputeRoutes_OneTwoCallerChannelCascadesAcrossMixMinus(t *testing.T) {
	// Plan §1.1: a single C=2 channel must flip every other source into mix
	// mode because each is fed back via a destination that requires the mixer.
	sources := []routeSource{
		{id: chID(1), channelID: chID(1)},
		{id: chID(2), channelID: chID(2)},
		{id: chID(3), channelID: chID(3)},
	}
	dests := []routeDest{
		{channelID: chID(1), sourceIDs: []snowflake.ID{chID(2), chID(3)}},
		{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1), chID(3)}},
		{channelID: chID(3), sourceIDs: []snowflake.ID{chID(1), chID(2)}},
	}
	counts := map[snowflake.ID]int{chID(1): 1, chID(2): 2, chID(3): 1}

	sourceModes, destMix := computeRoutes(sources, dests, counts)

	for _, s := range sources {
		if sourceModes[s.id] != routeMix {
			t.Errorf("cascade should force source %s into routeMix; got %v", s.id, sourceModes[s.id])
		}
	}
	for _, d := range dests {
		if !destMix[d.channelID] {
			t.Errorf("cascade should mark dest %s as mix; was copy", d.channelID)
		}
	}
}

func TestComputeRoutes_OffSourceStaysOffEvenWhenItsDestNeedsMix(t *testing.T) {
	// Source A: empty (C=0). Source B: C=2.
	// Destinations: dest A fed by [B], dest B fed by [A].
	// Cascade should: B→mix, dest A→mix. But A is off — no audio means no
	// race against the mixer, so A stays off.
	sources := []routeSource{
		{id: chID(10), channelID: chID(10)},
		{id: chID(20), channelID: chID(20)},
	}
	dests := []routeDest{
		{channelID: chID(10), sourceIDs: []snowflake.ID{chID(20)}},
		{channelID: chID(20), sourceIDs: []snowflake.ID{chID(10)}},
	}
	counts := map[snowflake.ID]int{chID(10): 0, chID(20): 2}

	sourceModes, destMix := computeRoutes(sources, dests, counts)

	if sourceModes[chID(10)] != routeOff {
		t.Errorf("off source must stay off through cascade; got %v", sourceModes[chID(10)])
	}
	if sourceModes[chID(20)] != routeMix {
		t.Errorf("C=2 source must be mix; got %v", sourceModes[chID(20)])
	}
	if !destMix[chID(10)] {
		t.Errorf("dest fed by mix source must be marked mix")
	}
	if destMix[chID(20)] {
		t.Errorf("dest fed only by off source should NOT be mix")
	}
}

func TestComputeRoutes_OneCallerSingleSourceNoCascade(t *testing.T) {
	// RaidModeOneCaller: single source feeds a single destination.
	sources := []routeSource{{id: chID(1), channelID: chID(1)}}
	dests := []routeDest{{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}}}

	for c := 0; c <= 3; c++ {
		counts := map[snowflake.ID]int{chID(1): c}
		sourceModes, destMix := computeRoutes(sources, dests, counts)
		var wantSource routeMode
		var wantMix bool
		switch {
		case c <= 0:
			wantSource, wantMix = routeOff, false
		case c == 1:
			wantSource, wantMix = routeCopy, false
		default:
			wantSource, wantMix = routeMix, true
		}
		if sourceModes[chID(1)] != wantSource {
			t.Errorf("c=%d: want source %v, got %v", c, wantSource, sourceModes[chID(1)])
		}
		if destMix[chID(2)] != wantMix {
			t.Errorf("c=%d: want destMix=%v, got %v", c, wantMix, destMix[chID(2)])
		}
	}
}

func TestSourceRouter_DebounceCoalescesBursts(t *testing.T) {
	// Plan §6: "Debounce coalesces 5 events in 100 ms into 1 Recompute."
	// Use a 30 ms debounce window so the test stays fast but the burst fits.
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &sourceSlot{id: chID(1), channelID: chID(1)}
	dst := &destSlot{channelID: chID(2), sources: []*sourceSlot{src}}
	src.feeds = []*destSlot{dst}

	r := newSourceRouter(chID(999), chID(7), counter, []*sourceSlot{src}, []*destSlot{dst})
	r.debounceWindow = 30 * time.Millisecond

	for range 5 {
		r.Debounce(chID(1))
		time.Sleep(2 * time.Millisecond) // < window, must coalesce
	}

	// Wait past the debounce window plus generous timer slack.
	time.Sleep(100 * time.Millisecond)

	if got := counter.calls.Load(); got != 1 {
		t.Errorf("debounce should coalesce 5 rapid events into 1 Recompute; got %d CountCallers calls", got)
	}
}

func TestSourceRouter_DebounceSeparateChannelsDoNotCoalesce(t *testing.T) {
	// Different source channels have independent debounce timers.
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1, chID(2): 1}}
	s1 := &sourceSlot{id: chID(1), channelID: chID(1)}
	s2 := &sourceSlot{id: chID(2), channelID: chID(2)}
	d := &destSlot{channelID: chID(99), sources: []*sourceSlot{s1, s2}}
	s1.feeds = []*destSlot{d}
	s2.feeds = []*destSlot{d}

	r := newSourceRouter(chID(1), chID(7), counter, []*sourceSlot{s1, s2}, []*destSlot{d})
	r.debounceWindow = 30 * time.Millisecond

	r.Debounce(chID(1))
	r.Debounce(chID(2))
	time.Sleep(100 * time.Millisecond)

	// Two recomputes, each counts both channels — so 4 CountCallers calls.
	if got := counter.calls.Load(); got != 4 {
		t.Errorf("separate channels should fire independently; want 4 counter calls, got %d", got)
	}
}

func TestSourceRouter_RecomputeIdempotent(t *testing.T) {
	// Calling Recompute twice with the same counts should not panic or shift
	// state.
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &sourceSlot{id: chID(1), channelID: chID(1)}
	dst := &destSlot{channelID: chID(2), sources: []*sourceSlot{src}}
	src.feeds = []*destSlot{dst}

	r := newSourceRouter(chID(1), chID(7), counter, []*sourceSlot{src}, []*destSlot{dst})
	r.Recompute()
	mode1 := src.mode
	r.Recompute()
	mode2 := src.mode
	if mode1 != mode2 {
		t.Errorf("idempotent Recompute changed mode: %v → %v", mode1, mode2)
	}
	if mode1 != routeCopy {
		t.Errorf("C=1 source must be routeCopy; got %v", mode1)
	}
}

func TestSourceRouter_PerUserSynthIDsAreUniqueAndStable(t *testing.T) {
	// §4.3 fix: each role-bearing user in a source channel must get their
	// own synth ID so the destination mixer registers them as independent
	// inputs (one SourceBuffer per user) instead of evicting each other in
	// a shared cap-3 ring.
	r := &sourceRouter{}
	s := &sourceSlot{id: chID(100), channelID: chID(200)}

	a1 := r.synthIDForLocked(s, chID(10))
	b1 := r.synthIDForLocked(s, chID(20))
	a2 := r.synthIDForLocked(s, chID(10)) // same user again

	if a1 == b1 {
		t.Errorf("different users must get different synth IDs; both = %v", a1)
	}
	if a1 != a2 {
		t.Errorf("same user must get stable synth ID; first=%v second=%v", a1, a2)
	}
	for _, id := range []snowflake.ID{a1, b1} {
		if uint64(id)>>63 != 1 {
			t.Errorf("synth ID must have bit 63 set (no collision with snowflakes); got %v (%b)", id, id)
		}
	}
}

func TestSourceRouter_UserSetChangeReinstallsEvenInSameMode(t *testing.T) {
	// Same mix mode across two recomputes, but the user set changes
	// (user 999 joins channel). The router must re-call buildInstall so the
	// new user's SourceBuffer gets registered.
	counter := &callerCount{
		users: map[snowflake.ID][]snowflake.ID{chID(1): {chID(11), chID(12)}},
	}
	// Provide a real (no-op) FanoutHandle so applyModes' nil-guard passes
	// and buildInstall gets called.
	src := &sourceSlot{id: chID(1), channelID: chID(1), handle: opus.NewFanoutHandle()}
	dst := &destSlot{channelID: chID(2), sources: []*sourceSlot{src}}
	src.feeds = []*destSlot{dst}

	var calls int
	var lastUsers []userBinding
	src.buildInstall = func(_ routeMode, users []userBinding) (opus.FanoutInstall, func()) {
		calls++
		lastUsers = users
		return opus.FanoutInstall{}, func() {}
	}

	r := newSourceRouter(chID(1), chID(7), counter, []*sourceSlot{src}, []*destSlot{dst})

	r.Recompute()
	// Routing is intentionally mocked via buildInstall; the slot's mode
	// transitioned from off → mix. We expect 1 call so far.
	if calls != 1 {
		t.Fatalf("first Recompute should fire buildInstall once; got %d", calls)
	}
	if len(lastUsers) != 2 {
		t.Fatalf("first install: want 2 user bindings, got %d", len(lastUsers))
	}

	// New user joins; mode stays routeMix (still ≥2 callers).
	counter.users[chID(1)] = []snowflake.ID{chID(11), chID(12), chID(999)}

	r.Recompute()
	if calls != 2 {
		t.Errorf("user-set change must trigger reinstall even in same mode; got %d total calls", calls)
	}
	if len(lastUsers) != 3 {
		t.Errorf("second install: want 3 user bindings, got %d", len(lastUsers))
	}

	// Same user set on a third Recompute: no reinstall.
	r.Recompute()
	if calls != 2 {
		t.Errorf("unchanged user set must NOT trigger reinstall; got %d total calls", calls)
	}
}

func TestSourceRouter_RecomputeAfterCloseBailsOut(t *testing.T) {
	// Defends against an AfterFunc that started before Close but was waiting
	// on mu when Close ran — the timer.Stop() returned false and the callback
	// continues. The closed flag prevents the late Recompute from touching
	// already-torn-down handles.
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &sourceSlot{id: chID(1), channelID: chID(1)}
	dst := &destSlot{channelID: chID(2), sources: []*sourceSlot{src}}
	src.feeds = []*destSlot{dst}

	r := newSourceRouter(chID(1), chID(7), counter, []*sourceSlot{src}, []*destSlot{dst})
	r.Close()
	r.Recompute()
	if got := counter.calls.Load(); got != 0 {
		t.Errorf("Recompute after Close must short-circuit; got %d EnumerateCallers calls", got)
	}
}

func TestSourceRouter_CloseStopsPendingTimers(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &sourceSlot{id: chID(1), channelID: chID(1)}
	dst := &destSlot{channelID: chID(2), sources: []*sourceSlot{src}}
	src.feeds = []*destSlot{dst}

	r := newSourceRouter(chID(1), chID(7), counter, []*sourceSlot{src}, []*destSlot{dst})
	r.debounceWindow = 200 * time.Millisecond
	r.Debounce(chID(1))
	r.Close()
	// Wait past when the timer would have fired.
	time.Sleep(300 * time.Millisecond)
	if got := counter.calls.Load(); got != 0 {
		t.Errorf("Close should cancel pending timers; got %d unexpected Recompute(s)", got)
	}
}
