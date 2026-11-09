package ally

import (
	"errors"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

func joinWithWrongCodes(t *testing.T, m *Manager, guildID snowflake.ID, n int) {
	t.Helper()
	for range n {
		if _, err := m.Join("WRONG123", guildID); err == nil || errors.Is(err, ErrJoinLockedOut) {
			t.Fatalf("wrong code: err = %v, want a plain not-found error", err)
		}
	}
}

func TestJoinLocksOutAfterRepeatedWrongCodes(t *testing.T) {
	t.Parallel()
	m := NewManager()
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	joinWithWrongCodes(t, m, guestID, joinMaxFailures)

	// The real code must be refused too, or the lockout tells a guesser which
	// attempt was right.
	if _, err := m.Join("TESTCODE", guestID); !errors.Is(err, ErrJoinLockedOut) {
		t.Fatalf("correct code while locked out: err = %v, want ErrJoinLockedOut", err)
	}
	if _, ok := m.GetByGuild(guestID); ok {
		t.Error("locked-out guild was attached to the session")
	}
}

func TestJoinLockoutExpires(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := NewManager()
	m.now = func() time.Time { return now }
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	joinWithWrongCodes(t, m, guestID, joinMaxFailures)
	now = now.Add(joinLockout + time.Second)

	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("join after the lockout: %v", err)
	}
}

func TestJoinLockoutIsPerGuild(t *testing.T) {
	t.Parallel()
	const otherGuest = snowflake.ID(3)
	m := NewManager()
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	joinWithWrongCodes(t, m, guestID, joinMaxFailures)

	if _, err := m.Join("TESTCODE", otherGuest); err != nil {
		t.Fatalf("an unrelated guild was locked out: %v", err)
	}
}

func TestJoinSuccessForgetsEarlierFailures(t *testing.T) {
	t.Parallel()
	m := NewManager()
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	joinWithWrongCodes(t, m, guestID, joinMaxFailures-1)
	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("join: %v", err)
	}
	m.RemoveGuest(guestID)

	joinWithWrongCodes(t, m, guestID, joinMaxFailures-1)
	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("failures before a success still counted: %v", err)
	}
}

func TestJoinFailuresOutsideTheWindowDoNotAccumulate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := NewManager()
	m.now = func() time.Time { return now }
	m.Create("TESTCODE", hostID, guild.RaidModeGuildCaller)

	for range joinMaxFailures {
		joinWithWrongCodes(t, m, guestID, 1)
		now = now.Add(joinLockout)
	}

	if _, err := m.Join("TESTCODE", guestID); err != nil {
		t.Fatalf("slow wrong guesses locked the guild out: %v", err)
	}
}
