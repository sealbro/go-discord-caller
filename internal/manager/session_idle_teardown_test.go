package manager

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// recordingVoiceManager records the guilds whose connection was looked up.
// GuildVoice.Leave starts with vm.GetConn(guildID) and cancels the guild's
// audio sender from the conn it returns, so a GetConn call is the observable
// entry point of the owner-side voice teardown.
type recordingVoiceManager struct {
	mu        sync.Mutex
	getConnOf []snowflake.ID
}

func (f *recordingVoiceManager) GetConn(guildID snowflake.ID) voice.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getConnOf = append(f.getConnOf, guildID)
	return nil
}

func (f *recordingVoiceManager) leftGuilds() []snowflake.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]snowflake.ID(nil), f.getConnOf...)
}

func (*recordingVoiceManager) HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate)   {}
func (*recordingVoiceManager) HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate) {}
func (*recordingVoiceManager) CreateConn(snowflake.ID) voice.Conn                     { return nil }
func (*recordingVoiceManager) Conns() iter.Seq[voice.Conn]                            { return func(func(voice.Conn) bool) {} }
func (*recordingVoiceManager) RemoveConn(snowflake.ID)                                {}
func (*recordingVoiceManager) Close(context.Context)                                  {}

// A session stopped by the idle watcher must leave the owner's voice conn, as
// /stop does — cancelling the context alone closes the provider but leaves the
// audio sender running, polling it 50×/s for the life of the process.
// Production 2026-09-26T22:44:01Z: 1.41M errors over the following 8 h.
func TestIdleStop_TearsDownOwnerVoice(t *testing.T) {
	t.Parallel()

	const ownerChannel = snowflake.ID(900)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannel)

	vm := &recordingVoiceManager{}
	session := &guild.Session{
		GuildID: testGuildID,
		Cancel:  func() {},
		Cleanup: func() {},
	}

	m := &Service{
		statuses:           map[snowflake.ID]*guild.Status{testGuildID: {Session: session}},
		store:              st,
		ownerClient:        &bot.Client{VoiceManager: vm, Caches: cache.New()},
		ownerBotID:         testBotID,
		poolSvc:            emptyPool{},
		sessions:           ally.NewManager(),
		reconnect:          newReconnectState(),
		sessionIdleTimeout: 10 * time.Millisecond,
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	m.activeRouters.Store(&empty)

	// The cache is empty, so the owner channel reads as unoccupied and the
	// watcher reaches its idle deadline on the first two ticks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.startSessionIdleWatcher(ctx, cancel, session)

	deadline := time.After(2 * time.Second)
	for {
		if len(vm.leftGuilds()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("idle stop must tear down the owner voice conn, but it never left the channel")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := vm.leftGuilds(); got[0] != testGuildID {
		t.Errorf("want owner voice teardown for guild %s, got %s", testGuildID, got[0])
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s := m.statuses[testGuildID].Session; s != nil {
		t.Error("idle stop must clear the session")
	}
}

// The watcher must only stop the session it was started for: the watched one can
// end and a fresh raid take its place before the stop runs, and stopping by guild
// alone would tear that new raid down seconds after it came up.
func TestIdleStop_LeavesANewerSessionAlone(t *testing.T) {
	t.Parallel()

	const ownerChannel = snowflake.ID(900)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannel)

	vm := &recordingVoiceManager{}
	watched := &guild.Session{GuildID: testGuildID, Cancel: func() {}, Cleanup: func() {}}
	newer := &guild.Session{GuildID: testGuildID, Cancel: func() {}, Cleanup: func() {}}

	m := &Service{
		statuses:           map[snowflake.ID]*guild.Status{testGuildID: {Session: watched}},
		store:              st,
		ownerClient:        &bot.Client{VoiceManager: vm, Caches: cache.New()},
		ownerBotID:         testBotID,
		poolSvc:            emptyPool{},
		sessions:           ally.NewManager(),
		reconnect:          newReconnectState(),
		sessionIdleTimeout: 10 * time.Millisecond,
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	m.activeRouters.Store(&empty)

	// The watched session ends and a new raid takes its place before the
	// watcher's stop runs.
	m.mu.Lock()
	m.statuses[testGuildID].Session = newer
	m.mu.Unlock()

	m.stopIdleSession(watched, func() {})

	m.mu.RLock()
	defer m.mu.RUnlock()
	if got := m.statuses[testGuildID].Session; got != newer {
		t.Error("idle stop tore down a session it was not watching")
	}
	if len(vm.leftGuilds()) != 0 {
		t.Error("idle stop left the newer session's voice conn")
	}
}
