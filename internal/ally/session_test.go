package ally

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

const (
	hostID  = snowflake.ID(1)
	guestID = snowflake.ID(2)
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	return NewManager().Create("TESTCODE", hostID, guild.RaidModeGuildCaller)
}

// HasCapturingPeers drives DestSlot.RelayFeed: it must ignore the asking guild
// and report only whether somebody ELSE can broadcast into the session.
func TestSession_HasCapturingPeers(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)

	if s.HasCapturingPeers(hostID) {
		t.Error("fresh session: host has no capturing peers")
	}

	s.SetCapturing(hostID)
	if s.HasCapturingPeers(hostID) {
		t.Error("a guild must not count itself as a peer")
	}
	if !s.HasCapturingPeers(guestID) {
		t.Error("guest must see the capturing host as a peer")
	}

	s.SetCapturing(guestID)
	if !s.HasCapturingPeers(hostID) {
		t.Error("host must see the capturing guest as a peer")
	}

	// A listener-only guest leaving must not strip the host's capture flag.
	s.RemoveGuild(guestID)
	if s.HasCapturingPeers(hostID) {
		t.Error("host should have no capturing peers once the guest left")
	}
	if !s.HasCapturingPeers(guestID) {
		t.Error("host capture flag must survive a guest teardown")
	}
}

// Membership changes must reach every registered router so relay-fed mixers
// unpause when a peer attaches (issue #51).
func TestSession_RouteObserversFireOnMembershipChange(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)

	fired := make(chan struct{}, 16)
	s.SetRouteObserver(hostID, func() { fired <- struct{}{} })

	waitFired := func(what string) {
		t.Helper()
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: observer never fired", what)
		}
	}

	s.AddListener(guestID, nil)
	waitFired("AddListener")

	s.SetCapturing(guestID)
	waitFired("SetCapturing")

	s.RemoveGuild(guestID)
	waitFired("RemoveGuild")
}

// A guild's own observer is dropped when it detaches, so a torn-down guest
// router is never called again.
func TestSession_RemoveGuildDropsItsObserver(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newTestSession(t)

		var guestCalls atomic.Int32
		s.SetRouteObserver(guestID, func() { guestCalls.Add(1) })

		s.RemoveGuild(guestID)
		synctest.Wait()
		if n := guestCalls.Load(); n != 0 {
			t.Errorf("removed guild's observer was notified of its own removal %d time(s)", n)
		}

		s.AddListener(snowflake.ID(3), nil)
		synctest.Wait()
		if n := guestCalls.Load(); n != 0 {
			t.Errorf("removed observer fired %d time(s) in all", n)
		}
	})
}

func TestSession_BroadcastReachesEveryOtherGuildAndDropsOnFullChannel(t *testing.T) {
	t.Parallel()
	const listenerID = snowflake.ID(3)
	s := newTestSession(t)

	type tagged struct {
		src snowflake.ID
		pkt []byte
	}
	var hostGot, guestGot []tagged
	s.AddReceiver(hostID, func(src snowflake.ID, pkt []byte) { hostGot = append(hostGot, tagged{src, pkt}) })
	s.AddReceiver(guestID, func(src snowflake.ID, pkt []byte) { guestGot = append(guestGot, tagged{src, pkt}) })
	listenerCh := make(chan []byte, 1)
	s.AddListener(listenerID, []chan<- []byte{listenerCh})

	s.BroadcastFromGuild(hostID, []byte{1, 2, 3})
	if len(hostGot) != 0 {
		t.Errorf("host received %d of its own packets", len(hostGot))
	}
	if len(guestGot) != 1 || guestGot[0].src != hostID || guestGot[0].pkt[0] != 1 {
		t.Errorf("guest receiver got %v, want the host's packet tagged with the host", guestGot)
	}
	select {
	case got := <-listenerCh:
		if len(got) != 3 || got[0] != 1 {
			t.Errorf("listener received %v, want the host's packet", got)
		}
	default:
		t.Error("listener received nothing")
	}

	s.BroadcastFromGuild(guestID, []byte{7})
	if len(hostGot) != 1 || hostGot[0].src != guestID {
		t.Errorf("host receiver got %v, want the guest's packet tagged with the guest", hostGot)
	}
	if len(guestGot) != 1 {
		t.Errorf("guest received its own packet back")
	}

	listenerCh <- []byte{9}
	done := make(chan struct{})
	go func() {
		s.BroadcastFromGuild(hostID, []byte{4})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on a full channel")
	}
}
