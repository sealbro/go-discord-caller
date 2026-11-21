package manager

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// Two /start commands can be in flight at once: handleStartVoiceRaid checks
// HasActiveSession and then does the work in a goroutine, and the gap between
// the two is the whole speaker-join sequence.
//
// commitSession re-checks under the lock, so only one raid is committed — but
// the loser then cleans up, and every resource it cleans up is shared per
// guild, not per session: the owner bot's voice conn, the speaker bots' voice
// conns, and the guild's entry in the ally session registry (the relay code is
// persistent per guild, so both raids create an ally session under the same
// code). The loser's cleanup therefore disconnects the winner's bots and
// unregisters its relay code, leaving a raid that reports itself active with
// nobody in voice and a code no guest can join.
func TestConcurrentStartVoiceRaid_LoserCleanupMustNotTearDownWinner(t *testing.T) {
	const (
		ownerChannelID   = snowflake.ID(900)
		speakerID        = snowflake.ID(500)
		speakerChannelID = snowflake.ID(901)
	)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannelID)
	st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	st.BindChannel(testGuildID, speakerID, speakerChannelID)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("concurrent_start_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Both starts must be past setupSpeakers — where snapshotSpeakers would
	// reject the second one — before either commits, or there is no race to
	// reproduce. The owner's CreateConn is the first thing that runs after the
	// speakers are in, so that is where the two are held.
	gate := newMeetingPoint(2)
	ownerConn := &fakeVoiceConn{channelID: ownerChannelID}
	ownerVM := &fakeVoiceManager{conn: ownerConn, onOpen: func() { gate.arrive(t) }}
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

	codes := make([]ally.Code, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range 2 {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			codes[i], errs[i] = m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
			gate.gaveUp()
		}()
	}
	wg.Wait()

	winners := 0
	var winningCode ally.Code
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			winningCode = codes[i]
		case errors.Is(err, ErrSessionExists):
		default:
			t.Fatalf("start %d failed for an unrelated reason: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent start must win, got %d", winners)
	}

	m.mu.RLock()
	committed := m.statuses[testGuildID].Session
	m.mu.RUnlock()
	if committed == nil {
		t.Fatal("the winning raid must stay committed")
	}

	session, ok := m.sessions.GetByGuild(testGuildID)
	if !ok {
		t.Errorf("the winning raid's relay code %q must stay registered; no guest can join a raid the loser unregistered", winningCode)
	} else {
		select {
		case <-session.Done():
			t.Error("the winning raid's ally session was closed by the losing start")
		default:
		}
	}

	if n := ownerConn.closeCount(); n != 0 {
		t.Errorf("owner voice conn closed %d time(s): the losing start disconnected the winning raid's owner bot", n)
	}
	if n := speakerConn.closeCount(); n != 0 {
		t.Errorf("speaker voice conn closed %d time(s): the losing start disconnected the winning raid's speaker", n)
	}
}
