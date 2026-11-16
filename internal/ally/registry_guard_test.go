package ally

import (
	"errors"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

func TestRemoveGuestIgnoresANewerSession(t *testing.T) {
	const otherHost = snowflake.ID(3)
	m := NewManager()
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)
	m.Create("OTHERCOD", otherHost, guild.RaidModeGuildCaller)

	old, err := m.Join("TESTCODE", guestID)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	m.RemoveGuest(guestID, old)
	newer, err := m.Join("OTHERCOD", guestID)
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}

	m.RemoveGuest(guestID, old)

	if got, ok := m.GetByGuild(guestID); !ok || got != newer {
		t.Error("a stale teardown removed the guest's newer registration")
	}
}

func TestOpenRefusesACodeHeldByAnotherHost(t *testing.T) {
	m := NewManager()
	held := m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	if _, err := m.Open("TESTCODE", guestID, guild.RaidModeGuildCaller); !errors.Is(err, ErrCodeInUse) {
		t.Fatalf("Open with another host's code: err = %v, want ErrCodeInUse", err)
	}
	if got, ok := m.GetByGuild(hostID); !ok || got != held {
		t.Error("the refused Open displaced the registered session")
	}
}

func TestOpenRefusesAHostThatIsAnotherGuildsGuest(t *testing.T) {
	m := NewManager()
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)
	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, err := m.Open("OTHERCOD", guestID, guild.RaidModeGuildCaller); !errors.Is(err, ErrCodeInUse) {
		t.Fatalf("Open for a guild attached as a guest: err = %v, want ErrCodeInUse", err)
	}
}

func TestOpenReplacesAndClosesAStaleSessionOfTheSameHost(t *testing.T) {
	m := NewManager()
	stale := m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	fresh, err := m.Open("TESTCODE", hostID, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	select {
	case <-stale.Done():
	default:
		t.Error("the replaced session was not closed: its guests would be orphaned")
	}
	if got, ok := m.GetByGuild(hostID); !ok || got != fresh {
		t.Error("the new session is not the registered one")
	}
}

func TestRemoveHostIgnoresAGuestGuild(t *testing.T) {
	m := NewManager()
	hosted := m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)
	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("join: %v", err)
	}

	m.RemoveHost(guestID)

	select {
	case <-hosted.Done():
		t.Error("RemoveHost for a guest guild closed another guild's session")
	default:
	}
	if _, ok := m.GetByGuild(guestID); !ok {
		t.Error("RemoveHost for a guest guild detached it")
	}
}

func TestRemoveLeavesANewerSessionOfTheSameHost(t *testing.T) {
	m := NewManager()
	old := m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)
	fresh, err := m.Open("TESTCODE", hostID, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	m.Remove(old)

	if got, ok := m.GetByGuild(hostID); !ok || got != fresh {
		t.Error("removing a stale session unregistered its replacement")
	}
}
