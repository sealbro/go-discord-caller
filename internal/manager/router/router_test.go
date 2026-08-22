package router

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// callerCount is a VoiceProbe that returns canned values per channel and
// counts how many times EnumerateCallers has been invoked. When users is
// nil it synthesises non-zero placeholder userIDs derived from channelID.
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
		out[i] = snowflake.ID(uint64(channelID)*1000 + uint64(i+1))
	}
	return out
}

// HasListeners defaults to true so cascade-focused tests don't need to wire
// listener fixtures.
func (c *callerCount) HasListeners(_ snowflake.ID) bool { return true }

func chID(i uint64) snowflake.ID { return snowflake.ID(i) }

func TestComputeRoutes_NPlus3MixMinusAlwaysRunsMixer(t *testing.T) {
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

	sourceModes, destMix := computeRoutes(sources, dests, counts, nil)

	for _, s := range sources {
		if sourceModes[s.id] != RouteMix {
			t.Errorf("N=3 mix-minus, all C=1: source %s must be RouteMix; got %v", s.id, sourceModes[s.id])
		}
	}
	for _, d := range dests {
		if !destMix[d.channelID] {
			t.Errorf("N=3 mix-minus: dest %s must be marked mix", d.channelID)
		}
	}
}

func TestComputeRoutes_N2MixMinusAllCopyWhenBothC1(t *testing.T) {
	sources := []routeSource{
		{id: chID(1), channelID: chID(1)},
		{id: chID(2), channelID: chID(2)},
	}
	dests := []routeDest{
		{channelID: chID(1), sourceIDs: []snowflake.ID{chID(2)}},
		{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}},
	}
	counts := map[snowflake.ID]int{chID(1): 1, chID(2): 1}

	sourceModes, destMix := computeRoutes(sources, dests, counts, nil)

	for _, s := range sources {
		if sourceModes[s.id] != RouteCopy {
			t.Errorf("N=2 mix-minus, all C=1: source %s must be RouteCopy; got %v", s.id, sourceModes[s.id])
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

	sourceModes, _ := computeRoutes(sources, dests, counts, nil)

	if sourceModes[chID(1)] != RouteOff {
		t.Errorf("empty channel: want RouteOff, got %v", sourceModes[chID(1)])
	}
	if sourceModes[chID(2)] != RouteCopy {
		t.Errorf("solo channel: want RouteCopy, got %v", sourceModes[chID(2)])
	}
}

func TestComputeRoutes_OneTwoCallerChannelCascadesAcrossMixMinus(t *testing.T) {
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

	sourceModes, destMix := computeRoutes(sources, dests, counts, nil)

	for _, s := range sources {
		if sourceModes[s.id] != RouteMix {
			t.Errorf("cascade should force source %s into RouteMix; got %v", s.id, sourceModes[s.id])
		}
	}
	for _, d := range dests {
		if !destMix[d.channelID] {
			t.Errorf("cascade should mark dest %s as mix", d.channelID)
		}
	}
}

func TestComputeRoutes_OffSourceStaysOffEvenWhenItsDestNeedsMix(t *testing.T) {
	sources := []routeSource{
		{id: chID(10), channelID: chID(10)},
		{id: chID(20), channelID: chID(20)},
	}
	dests := []routeDest{
		{channelID: chID(10), sourceIDs: []snowflake.ID{chID(20)}},
		{channelID: chID(20), sourceIDs: []snowflake.ID{chID(10)}},
	}
	counts := map[snowflake.ID]int{chID(10): 0, chID(20): 2}

	sourceModes, destMix := computeRoutes(sources, dests, counts, nil)

	if sourceModes[chID(10)] != RouteOff {
		t.Errorf("off source must stay off through cascade; got %v", sourceModes[chID(10)])
	}
	if sourceModes[chID(20)] != RouteMix {
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
	sources := []routeSource{{id: chID(1), channelID: chID(1)}}
	dests := []routeDest{{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}}}

	for c := 0; c <= 3; c++ {
		counts := map[snowflake.ID]int{chID(1): c}
		sourceModes, destMix := computeRoutes(sources, dests, counts, nil)
		var wantSource RouteMode
		var wantMix bool
		switch {
		case c <= 0:
			wantSource, wantMix = RouteOff, false
		case c == 1:
			wantSource, wantMix = RouteCopy, false
		default:
			wantSource, wantMix = RouteMix, true
		}
		if sourceModes[chID(1)] != wantSource {
			t.Errorf("c=%d: want source %v, got %v", c, wantSource, sourceModes[chID(1)])
		}
		if destMix[chID(2)] != wantMix {
			t.Errorf("c=%d: want destMix=%v, got %v", c, wantMix, destMix[chID(2)])
		}
	}
}

func TestRouter_DebounceCoalescesBursts(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}

	r := New(chID(999), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})
	r.debounceWindow = 30 * time.Millisecond

	for range 5 {
		r.Debounce(chID(1))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	if got := counter.calls.Load(); got != 1 {
		t.Errorf("debounce should coalesce 5 rapid events into 1 Recompute; got %d calls", got)
	}
}

func TestRouter_DebounceSeparateChannelsDoNotCoalesce(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1, chID(2): 1}}
	s1 := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	s2 := &SourceSlot{ID: chID(2), ChannelID: chID(2)}
	d := &DestSlot{ChannelID: chID(99), Sources: []*SourceSlot{s1, s2}}
	s1.Feeds = []*DestSlot{d}
	s2.Feeds = []*DestSlot{d}

	r := New(chID(1), chID(7), counter, []*SourceSlot{s1, s2}, []*DestSlot{d})
	r.debounceWindow = 30 * time.Millisecond

	r.Debounce(chID(1))
	r.Debounce(chID(2))
	time.Sleep(100 * time.Millisecond)

	if got := counter.calls.Load(); got != 4 {
		t.Errorf("separate channels should fire independently; want 4 counter calls, got %d", got)
	}
}

func TestRouter_RecomputeIdempotent(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}

	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})
	r.Recompute()
	mode1 := src.activeMode
	r.Recompute()
	mode2 := src.activeMode
	if mode1 != mode2 {
		t.Errorf("idempotent Recompute changed mode: %v → %v", mode1, mode2)
	}
	if mode1 != RouteCopy {
		t.Errorf("C=1 source must be RouteCopy; got %v", mode1)
	}
}

func TestRouter_PerUserSynthIDsAreUniqueAndStable(t *testing.T) {
	r := &Router{}
	s := &SourceSlot{ID: chID(100), ChannelID: chID(200)}

	a1 := r.synthIDForLocked(s, chID(10))
	b1 := r.synthIDForLocked(s, chID(20))
	a2 := r.synthIDForLocked(s, chID(10))

	if a1 == b1 {
		t.Errorf("different users must get different synth IDs; both = %v", a1)
	}
	if a1 != a2 {
		t.Errorf("same user must get stable synth ID; first=%v second=%v", a1, a2)
	}
	for _, id := range []snowflake.ID{a1, b1} {
		if uint64(id)>>63 != 1 {
			t.Errorf("synth ID must have bit 63 set; got %v (%b)", id, id)
		}
	}
}

func TestRouter_UserSetChangeReinstallsEvenInSameMode(t *testing.T) {
	counter := &callerCount{
		users: map[snowflake.ID][]snowflake.ID{chID(1): {chID(11), chID(12)}},
	}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1), Handle: opus.NewFanoutHandle()}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}

	var calls int
	var lastUsers []UserBinding
	src.BuildInstall = func(_ RouteMode, users []UserBinding) (opus.FanoutInstall, func()) {
		calls++
		lastUsers = users
		return opus.FanoutInstall{}, func() {}
	}

	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})

	r.Recompute()
	if calls != 1 {
		t.Fatalf("first Recompute should fire BuildInstall once; got %d", calls)
	}
	if len(lastUsers) != 2 {
		t.Fatalf("first install: want 2 user bindings, got %d", len(lastUsers))
	}

	counter.users[chID(1)] = []snowflake.ID{chID(11), chID(12), chID(999)}
	r.Recompute()
	if calls != 2 {
		t.Errorf("user-set change must trigger reinstall even in same mode; got %d total calls", calls)
	}
	if len(lastUsers) != 3 {
		t.Errorf("second install: want 3 user bindings, got %d", len(lastUsers))
	}

	r.Recompute()
	if calls != 2 {
		t.Errorf("unchanged user set must NOT trigger reinstall; got %d total calls", calls)
	}
}

func TestRouter_TransitionRecorderFiresOnlyOnModeChange(t *testing.T) {
	counter := &callerCount{
		users: map[snowflake.ID][]snowflake.ID{chID(1): {chID(11), chID(12)}},
	}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1), Handle: opus.NewFanoutHandle()}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}
	src.BuildInstall = func(_ RouteMode, _ []UserBinding) (opus.FanoutInstall, func()) {
		return opus.FanoutInstall{}, func() {}
	}

	type transition struct{ from, to RouteMode }
	var got []transition
	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst}).
		WithTransitionRecorder(func(from, to RouteMode) {
			got = append(got, transition{from, to})
		})

	r.Recompute() // off → mix
	counter.users[chID(1)] = []snowflake.ID{chID(11), chID(12), chID(13)}
	r.Recompute() // mix → mix (user-set change; not a transition)
	counter.users[chID(1)] = []snowflake.ID{chID(11)}
	r.Recompute() // mix → copy
	counter.users[chID(1)] = nil
	r.Recompute() // copy → off

	want := []transition{
		{RouteOff, RouteMix},
		{RouteMix, RouteCopy},
		{RouteCopy, RouteOff},
	}
	if len(got) != len(want) {
		t.Fatalf("transition count: want %d, got %d (%v)", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("transition[%d]: want %v→%v, got %v→%v", i, w.from, w.to, got[i].from, got[i].to)
		}
	}
}

func TestRouter_RecomputeAfterCloseBailsOut(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}

	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})
	r.Close()
	r.Recompute()
	if got := counter.calls.Load(); got != 0 {
		t.Errorf("Recompute after Close must short-circuit; got %d calls", got)
	}
}

func TestRouter_CloseStopsPendingTimers(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 1}}
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	dst := &DestSlot{ChannelID: chID(2), Sources: []*SourceSlot{src}}
	src.Feeds = []*DestSlot{dst}

	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})
	r.debounceWindow = 200 * time.Millisecond
	r.Debounce(chID(1))
	r.Close()
	time.Sleep(300 * time.Millisecond)
	if got := counter.calls.Load(); got != 0 {
		t.Errorf("Close should cancel pending timers; got %d Recompute(s)", got)
	}
}

// A relay-fed destination must be marked mix even when no local source is
// live, and any local copy-mode source feeding it must be promoted to mix so
// its raw Opus writer cannot race the mixer sink on the shared ChOuts.
// Regression guard for issue #51.
func TestComputeRoutes_RelayFedDestRunsWithoutLocalCallers(t *testing.T) {
	sources := []routeSource{{id: chID(1), channelID: chID(1)}}
	dests := []routeDest{{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}}}
	relayFed := map[snowflake.ID]bool{chID(2): true}

	for _, c := range []int{0, 1} {
		counts := map[snowflake.ID]int{chID(1): c}
		sourceModes, destMix := computeRoutes(sources, dests, counts, relayFed)
		if !destMix[chID(2)] {
			t.Errorf("c=%d: relay-fed dest must be marked mix so guest audio is not dropped", c)
		}
		wantSource := RouteOff
		if c == 1 {
			wantSource = RouteMix
		}
		if sourceModes[chID(1)] != wantSource {
			t.Errorf("c=%d: want source %v, got %v", c, wantSource, sourceModes[chID(1)])
		}
	}
}

// Without a relay feed the dest keeps the cheap copy/off route.
func TestComputeRoutes_NoRelayFeedKeepsCopyRoute(t *testing.T) {
	sources := []routeSource{{id: chID(1), channelID: chID(1)}}
	dests := []routeDest{{channelID: chID(2), sourceIDs: []snowflake.ID{chID(1)}}}
	counts := map[snowflake.ID]int{chID(1): 1}

	sourceModes, destMix := computeRoutes(sources, dests, counts, map[snowflake.ID]bool{chID(2): false})
	if destMix[chID(2)] {
		t.Error("dest without relay feed must not be forced into mix")
	}
	if sourceModes[chID(1)] != RouteCopy {
		t.Errorf("want RouteCopy, got %v", sourceModes[chID(1)])
	}
}

// The RelayFeed predicate is re-evaluated on every Recompute, so a mixer
// paused while no peer was attached resumes as soon as one is.
func TestRouter_RelayFeedTogglesDestPause(t *testing.T) {
	counter := &callerCount{counts: map[snowflake.ID]int{chID(1): 0}}
	mx, err := opus.NewMixer(telemetry.OpusRecorder{})
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	var fed atomic.Bool
	src := &SourceSlot{ID: chID(1), ChannelID: chID(1)}
	dst := &DestSlot{
		ChannelID: chID(2),
		Mixer:     mx,
		Sources:   []*SourceSlot{src},
		ChOuts:    []chan<- []byte{make(chan []byte, 1)},
		RelayFeed: fed.Load,
	}
	src.Feeds = []*DestSlot{dst}

	r := New(chID(1), chID(7), counter, []*SourceSlot{src}, []*DestSlot{dst})

	r.Recompute()
	if !mx.Paused() {
		t.Error("no local callers and no relay feed: mixer should be paused")
	}

	fed.Store(true)
	r.Recompute()
	if mx.Paused() {
		t.Error("relay feed live: mixer must run so relayed guest audio is emitted")
	}

	fed.Store(false)
	r.Recompute()
	if !mx.Paused() {
		t.Error("relay feed gone: mixer should pause again")
	}
}
