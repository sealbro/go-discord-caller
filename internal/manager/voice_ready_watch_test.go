package manager

import (
	"context"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

const (
	testGuildID = snowflake.ID(1430511050704289835)
	testBotID   = snowflake.ID(1484911601210495038)
)

// newTestService builds a Service with only the fields reapplyAfterVoiceReady
// touches. It deliberately skips metrics/store/pool: the re-identify repair path
// reads m.statuses and m.reconnect and nothing else.
func newTestService(withSession bool) *Service {
	m := &Service{
		statuses:  make(map[snowflake.ID]*guild.Status),
		reconnect: newReconnectState(),
	}
	st := &guild.Status{}
	if withSession {
		st.Session = &guild.Session{GuildID: testGuildID}
	}
	m.statuses[testGuildID] = st
	return m
}

// countingApplier returns a reconnectApplier that records how many times it ran.
func countingApplier(calls *int, mu *sync.Mutex) reconnectApplier {
	return func(_ context.Context, _ voice.Conn) {
		mu.Lock()
		*calls++
		mu.Unlock()
	}
}

func TestReapplyAfterVoiceReady_RunsApplierDuringActiveSession(t *testing.T) {
	m := newTestService(true)
	var mu sync.Mutex
	calls := 0
	m.storeApplier(testGuildID, testBotID, countingApplier(&calls, &mu))

	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("applier calls = %d, want 1", calls)
	}
}

func TestReapplyAfterVoiceReady_NoopWithoutActiveSession(t *testing.T) {
	// A voice gateway can re-identify after the session was torn down (the conn
	// outlives the session until Leave completes). Re-wiring then would resurrect
	// audio into a channel the bot is on its way out of.
	m := newTestService(false)
	var mu sync.Mutex
	calls := 0
	m.storeApplier(testGuildID, testBotID, countingApplier(&calls, &mu))

	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("applier calls = %d, want 0 (no active session)", calls)
	}
}

func TestReapplyAfterVoiceReady_NoopWithoutApplier(t *testing.T) {
	m := newTestService(true)
	// No applier stored — must not panic.
	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)
}

func TestReapplyAfterVoiceReady_SkipsWhenReconnectInFlight(t *testing.T) {
	// ReconnectBotChannel holds the same guard while it rejoins and re-applies.
	// A Ready arriving mid-rejoin must not race it on the same conn.
	m := newTestService(true)
	var mu sync.Mutex
	calls := 0
	m.storeApplier(testGuildID, testBotID, countingApplier(&calls, &mu))

	if !m.reconnect.tryLock(testGuildID, testBotID) {
		t.Fatal("failed to acquire reconnect guard")
	}
	defer m.reconnect.unlock(testGuildID, testBotID)

	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("applier calls = %d, want 0 (reconnect in flight)", calls)
	}
}

func TestReapplyAfterVoiceReady_ReleasesGuard(t *testing.T) {
	// Repeated re-identifies are expected over a long session; the guard must not
	// leak, or the first repair would be the only one that ever runs.
	m := newTestService(true)
	var mu sync.Mutex
	calls := 0
	m.storeApplier(testGuildID, testBotID, countingApplier(&calls, &mu))

	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)
	m.reapplyAfterVoiceReady(testGuildID, testBotID, nil)

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("applier calls = %d, want 2", calls)
	}
}
