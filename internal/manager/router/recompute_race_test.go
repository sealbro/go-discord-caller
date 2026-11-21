package router

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// gatedProbe parks its first EnumerateCallers call after reading the callers,
// so a test can change the world while a Recompute holds an already-taken
// snapshot.
type gatedProbe struct {
	*captureProbe
	parked  atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGatedProbe() *gatedProbe {
	return &gatedProbe{captureProbe: newCaptureProbe(), entered: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedProbe) EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID {
	users := p.captureProbe.EnumerateCallers(channelID, roleID)
	if p.parked.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.release
	}
	return users
}

func recomputeAsync(r *Router) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Recompute()
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

// A Recompute that snapshotted before a caller arrived must not publish its
// stale "nobody here" result after a later Recompute already saw the caller.
func TestStaleRecomputeDoesNotOverwriteNewerOne(t *testing.T) {
	const (
		botID  = snowflake.ID(100)
		chanID = snowflake.ID(200)
		destID = snowflake.ID(300)
		userA  = snowflake.ID(11)
	)
	probe := newGatedProbe()
	log := &captureLog{}
	src := &SourceSlot{ID: botID, ChannelID: chanID}
	dest := &DestSlot{ChannelID: destID, Sources: []*SourceSlot{src}, ChOuts: []chan<- []byte{make(chan []byte, 1)}}
	src.Feeds = []*DestSlot{dest}
	r := New(snowflake.ID(1), snowflake.ID(9), probe, []*SourceSlot{src}, []*DestSlot{dest}).
		WithCaptureObserver(log.observe)
	defer r.Close()

	first := recomputeAsync(r)
	<-probe.entered
	probe.set(chanID, userA)
	second := recomputeAsync(r)

	// Recomputes are serialised, so the second one waits on a mutex here and
	// this times out; without that it gets the chance to finish first, which is
	// the interleaving that publishes the stale result. A mutex wait is not
	// something synctest can wait out, hence the real timer.
	select {
	case <-second:
	case <-time.After(100 * time.Millisecond):
	}
	close(probe.release)
	waitDone(t, first, "first recompute")
	waitDone(t, second, "second recompute")

	if !log.capturing(botID) {
		t.Fatal("capture state is off although a caller is present: a stale recompute was applied after a newer one")
	}
	r.mu.Lock()
	mode := src.activeMode
	r.mu.Unlock()
	if mode != RouteCopy {
		t.Fatalf("source mode = %v, want copy", mode)
	}
}

func TestRecomputeInFlightDuringCloseInstallsNothing(t *testing.T) {
	const (
		botID  = snowflake.ID(100)
		chanID = snowflake.ID(200)
		destID = snowflake.ID(300)
		userA  = snowflake.ID(11)
	)
	probe := newGatedProbe()
	probe.set(chanID, userA)
	var installs int
	var installsMu sync.Mutex
	src := &SourceSlot{
		ID:        botID,
		ChannelID: chanID,
		Handle:    opus.NewFanoutHandle(),
		BuildInstall: func(RouteMode, []UserBinding) (opus.FanoutInstall, func()) {
			installsMu.Lock()
			installs++
			installsMu.Unlock()
			return opus.FanoutInstall{}, func() {}
		},
	}
	dest := &DestSlot{ChannelID: destID, Sources: []*SourceSlot{src}, ChOuts: []chan<- []byte{make(chan []byte, 1)}}
	src.Feeds = []*DestSlot{dest}
	r := New(snowflake.ID(1), snowflake.ID(9), probe, []*SourceSlot{src}, []*DestSlot{dest})

	inFlight := recomputeAsync(r)
	<-probe.entered
	r.Close()
	close(probe.release)
	waitDone(t, inFlight, "in-flight recompute")

	installsMu.Lock()
	defer installsMu.Unlock()
	if installs != 0 {
		t.Fatalf("BuildInstall ran %d time(s) after Close: the install has no teardown and leaks mixer inputs", installs)
	}
}
