package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// A raid is only committed once every bot is in, so for the whole startup —
// seconds, a Discord round trip per bot — /stop finds nothing in m.statuses and
// reports that no raid is running. The raid then comes up anyway, bots and all,
// and the manager who asked for it to stop has to ask again once it has
// finished. Stopping a raid that is still starting must abort it.
func TestStopVoiceRaid_AbortsAStartStillInFlight(t *testing.T) {
	const (
		ownerChannelID   = snowflake.ID(900)
		speakerID        = snowflake.ID(500)
		speakerChannelID = snowflake.ID(901)
	)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannelID)
	st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	st.BindChannel(testGuildID, speakerID, speakerChannelID)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("stop_during_start_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// The start is held at the owner's CreateConn — past the speaker joins,
	// short of the commit — which is the window a /stop lands in.
	reached := make(chan struct{})
	release := make(chan struct{})
	ownerConn := &fakeVoiceConn{channelID: ownerChannelID}
	ownerVM := &fakeVoiceManager{conn: ownerConn, onOpen: func() {
		close(reached)
		<-release
	}}
	speakerConn := &fakeVoiceConn{channelID: speakerChannelID}
	speakerVM := &fakeVoiceManager{conn: speakerConn}

	m := &Service{
		statuses: map[snowflake.ID]*guild.Status{
			testGuildID: {
				GuildID:     testGuildID,
				OwnerUserID: testBotID,
				Speakers: map[snowflake.ID]*guild.Speaker{
					speakerID: {ID: speakerID, Enabled: true},
				},
			},
		},
		store:       st,
		poolSvc:     onePool{id: speakerID, client: &bot.Client{VoiceManager: speakerVM, Caches: cache.New(), Rest: stubRest{}}},
		ownerClient: &bot.Client{VoiceManager: ownerVM, Caches: cachesWithChannels(t, testGuildID, ownerChannelID, speakerChannelID), Rest: stubRest{}},
		ownerBotID:  testBotID,
		sessions:    ally.NewManager(),
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	m.activeRouters.Store(&empty)

	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	started := make(chan error, 1)
	go func() {
		_, err := m.StartVoiceRaid(startCtx, testGuildID, startCancel, guild.RaidModeOneCaller)
		started <- err
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("start never reached the owner join")
	}

	stopErr := m.StopVoiceRaid(context.Background(), testGuildID)
	close(release)

	select {
	case err := <-started:
		if err == nil {
			t.Error("a start that was stopped must not report success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start never returned")
	}

	if errors.Is(stopErr, ErrNoActiveSession) {
		t.Error("/stop reported no active raid while one was starting, and the raid came up anyway")
	} else if stopErr != nil {
		t.Errorf("stop failed for an unrelated reason: %v", stopErr)
	}

	m.mu.RLock()
	committed := m.statuses[testGuildID].Session
	m.mu.RUnlock()
	if committed != nil {
		t.Error("a stopped start must not commit its session")
	}
	if _, ok := m.sessions.GetByGuild(testGuildID); ok {
		t.Error("a stopped start must not leave its ally session registered")
	}
	if n := ownerConn.closeCount(); n == 0 {
		t.Error("a stopped start must leave the owner bot's voice channel")
	}
	if n := speakerConn.closeCount(); n == 0 {
		t.Error("a stopped start must leave the speaker bot's voice channel")
	}
}

// A /stop that lands while the speakers are still joining fails every join with
// the start's own cancellation. Reporting that as "no speakers joined: verify
// speaker channels are bound…" sends whoever reads it to check a setup that is
// fine; the start was cancelled, and the error has to say so.
func TestStopDuringSpeakerJoins_ReportsCancellationNotMissingSpeakers(t *testing.T) {
	const (
		ownerChannelID   = snowflake.ID(900)
		speakerID        = snowflake.ID(500)
		speakerChannelID = snowflake.ID(901)
	)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannelID)
	st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	st.BindChannel(testGuildID, speakerID, speakerChannelID)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("stop_during_start_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// The speaker's handshake hangs, as disgo's Open does until its context ends.
	joining := make(chan struct{})
	speakerConn := &fakeVoiceConn{channelID: speakerChannelID, open: func(ctx context.Context) error {
		close(joining)
		<-ctx.Done()
		return ctx.Err()
	}}
	ownerConn := &fakeVoiceConn{channelID: ownerChannelID}

	m := &Service{
		statuses: map[snowflake.ID]*guild.Status{
			testGuildID: {
				GuildID:     testGuildID,
				OwnerUserID: testBotID,
				Speakers: map[snowflake.ID]*guild.Speaker{
					speakerID: {ID: speakerID, Enabled: true},
				},
			},
		},
		store:       st,
		poolSvc:     onePool{id: speakerID, client: &bot.Client{VoiceManager: &fakeVoiceManager{conn: speakerConn}, Caches: cache.New(), Rest: stubRest{}}},
		ownerClient: &bot.Client{VoiceManager: &fakeVoiceManager{conn: ownerConn}, Caches: cachesWithChannels(t, testGuildID, ownerChannelID, speakerChannelID), Rest: stubRest{}},
		ownerBotID:  testBotID,
		sessions:    ally.NewManager(),
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	m.activeRouters.Store(&empty)

	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	started := make(chan error, 1)
	go func() {
		_, err := m.StartVoiceRaid(startCtx, testGuildID, startCancel, guild.RaidModeOneCaller)
		started <- err
	}()

	select {
	case <-joining:
	case <-time.After(5 * time.Second):
		t.Fatal("start never reached the speaker join")
	}
	if err := m.StopVoiceRaid(context.Background(), testGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}

	select {
	case err := <-started:
		if errors.Is(err, ErrNoSpeakers) {
			t.Errorf("a start stopped during the speaker joins reported %q", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("start error = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start never returned")
	}
}

// A raid only becomes active once every bot is in, and for the seconds before
// that /setup let the caller role, the channels and the relay code change under
// the start: a speaker that later reconnected went to the newly bound channel,
// not the one the raid was wired for. The settings stay locked from the moment
// a start begins until its teardown is over.
func TestSetupLocked_WhileARaidIsStillStarting(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reached := make(chan struct{})
	release := make(chan struct{})
	f.ownerVM.onOpen = func() {
		close(reached)
		<-release
	}

	if f.m.SetupLocked(testGuildID) {
		t.Fatal("settings locked before any raid")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
		started <- err
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("start never reached the owner join")
	}
	locked := f.m.SetupLocked(testGuildID)
	close(release)
	if err := <-started; err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	if !locked {
		t.Error("settings were unlocked while the raid was still starting")
	}

	if !f.m.SetupLocked(testGuildID) {
		t.Error("settings unlocked while the raid is active")
	}
	if err := f.m.StopVoiceRaid(context.Background(), testGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	if f.m.SetupLocked(testGuildID) {
		t.Error("settings still locked after the raid stopped")
	}
}
