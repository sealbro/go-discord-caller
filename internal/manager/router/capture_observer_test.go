package router

import (
	"slices"
	"sync"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// captureProbe is a VoiceProbe whose per-channel caller list the test mutates
// between Recompute calls, standing in for people joining, leaving, and gaining
// or losing the capture role.
type captureProbe struct {
	mu       sync.Mutex
	callers  map[snowflake.ID][]snowflake.ID
	listener bool
}

func newCaptureProbe() *captureProbe {
	return &captureProbe{callers: map[snowflake.ID][]snowflake.ID{}, listener: true}
}

func (p *captureProbe) set(channelID snowflake.ID, users ...snowflake.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.callers[channelID] = users
}

func (p *captureProbe) EnumerateCallers(channelID, _ snowflake.ID) []snowflake.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.callers[channelID])
}

func (p *captureProbe) HasListeners(snowflake.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listener
}

// captureLog records every capture map the router emits.
type captureLog struct {
	mu   sync.Mutex
	last map[snowflake.ID]bool
	n    int
}

func (l *captureLog) observe(capturing map[snowflake.ID]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = capturing
	l.n++
}

func (l *captureLog) capturing(id snowflake.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[id]
}

func (l *captureLog) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// TestCaptureObserverFollowsCallerPresence walks the full lifecycle of one
// capture source as people come and go from its channel. Each step is one of
// the cases the deaf controller must react to; `capturing == false` is exactly
// the condition under which the bot should end up server-deafened.
func TestCaptureObserverFollowsCallerPresence(t *testing.T) {
	const (
		botID   = snowflake.ID(100)
		chanID  = snowflake.ID(200)
		destID  = snowflake.ID(300)
		userA   = snowflake.ID(11)
		userB   = snowflake.ID(12)
		roleID  = snowflake.ID(9)
		guildID = snowflake.ID(1)
	)

	probe := newCaptureProbe()
	log := &captureLog{}

	src := &SourceSlot{ID: botID, ChannelID: chanID}
	dest := &DestSlot{ChannelID: destID, Sources: []*SourceSlot{src}, ChOuts: []chan<- []byte{make(chan []byte, 1)}}
	src.Feeds = []*DestSlot{dest}

	r := New(guildID, roleID, probe, []*SourceSlot{src}, []*DestSlot{dest}).
		WithCaptureObserver(log.observe)
	defer r.Close()

	steps := []struct {
		name  string
		setup func()
		want  bool
	}{
		// Case 2: capture bot joins a channel with nobody holding the role.
		{"empty channel at start", func() { probe.set(chanID) }, false},
		// Case 6: a member who already has the role joins the channel.
		{"role-bearing member joins", func() { probe.set(chanID, userA) }, true},
		// Case 4: that member loses the role and nobody else has it.
		{"only caller loses role", func() { probe.set(chanID) }, false},
		// Case 5: the role is granted again.
		{"role granted again", func() { probe.set(chanID, userA) }, true},
		// Missed case: a second caller arrives (must stay capturing).
		{"second caller joins", func() { probe.set(chanID, userA, userB) }, true},
		// Missed case: one of two loses the role — the other still holds it,
		// so the bot must NOT be deafened. This is where an off-by-one lives.
		{"one of two loses role", func() { probe.set(chanID, userB) }, true},
		// Missed case: the last caller leaves the channel entirely rather than
		// losing the role — a different event path, same required outcome.
		{"last caller leaves channel", func() { probe.set(chanID) }, false},
	}

	for _, step := range steps {
		step.setup()
		r.Recompute()
		if got := log.capturing(botID); got != step.want {
			t.Errorf("%s: capturing = %v, want %v", step.name, got, step.want)
		}
	}
}

// TestCaptureObserverReportsEveryRecompute pins that the observer receives the
// full map each time rather than a delta. The deaf controller relies on this to
// converge after a missed update or a voice reconnect instead of drifting.
func TestCaptureObserverReportsEveryRecompute(t *testing.T) {
	const (
		botID   = snowflake.ID(100)
		chanID  = snowflake.ID(200)
		destID  = snowflake.ID(300)
		userA   = snowflake.ID(11)
		guildID = snowflake.ID(1)
	)

	probe := newCaptureProbe()
	probe.set(chanID, userA)
	log := &captureLog{}

	src := &SourceSlot{ID: botID, ChannelID: chanID}
	dest := &DestSlot{ChannelID: destID, Sources: []*SourceSlot{src}, ChOuts: []chan<- []byte{make(chan []byte, 1)}}
	src.Feeds = []*DestSlot{dest}

	r := New(guildID, snowflake.ID(9), probe, []*SourceSlot{src}, []*DestSlot{dest}).
		WithCaptureObserver(log.observe)
	defer r.Close()

	// Three recomputes with no state change at all.
	r.Recompute()
	r.Recompute()
	r.Recompute()

	if got := log.calls(); got != 3 {
		t.Errorf("observer called %d times, want 3 (full state every recompute, not deltas)", got)
	}
	if !log.capturing(botID) {
		t.Error("source should still report capturing")
	}
}

// TestCaptureObserverNilSafe guards the guilds where nothing drives deaf state
// (no permission, or a pipeline with no DeafSink): Setup.CaptureObserver
// returns nil and the router must not panic.
func TestCaptureObserverNilSafe(t *testing.T) {
	probe := newCaptureProbe()
	src := &SourceSlot{ID: snowflake.ID(100), ChannelID: snowflake.ID(200)}
	r := New(snowflake.ID(1), snowflake.ID(9), probe, []*SourceSlot{src}, nil).
		WithCaptureObserver(nil)
	defer r.Close()
	r.Recompute() // must not panic
}
