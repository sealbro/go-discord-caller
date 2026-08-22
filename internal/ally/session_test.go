package ally

import (
	"testing"
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

	s.AddGuild(guestID, nil)
	waitFired("AddGuild")

	s.SetCapturing(guestID)
	waitFired("SetCapturing")

	s.RemoveGuild(guestID)
	waitFired("RemoveGuild")
}

// A guild's own observer is dropped when it detaches, so a torn-down guest
// router is never called again.
func TestSession_RemoveGuildDropsItsObserver(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)

	var guestCalls int
	done := make(chan struct{})
	s.SetRouteObserver(guestID, func() { guestCalls++; close(done) })

	s.RemoveGuild(guestID)
	select {
	case <-done:
		t.Error("removed guild's observer must not be notified")
	case <-time.After(100 * time.Millisecond):
	}

	s.AddGuild(snowflake.ID(3), nil)
	time.Sleep(100 * time.Millisecond)
	if guestCalls != 0 {
		t.Errorf("removed observer fired %d time(s) on a later membership change", guestCalls)
	}
}
