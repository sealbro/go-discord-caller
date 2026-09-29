package manager

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// fakeVoiceConn is a voice.Conn that records closes. Everything the raid setup
// calls on a conn either succeeds or is a no-op; the embedded interface panics
// on anything else, which keeps the fake honest about what the path touches.
type fakeVoiceConn struct {
	voice.Conn
	channelID snowflake.ID

	mu     sync.Mutex
	closes int
}

func (c *fakeVoiceConn) Open(context.Context, snowflake.ID, bool, bool) error { return nil }

func (c *fakeVoiceConn) Close(context.Context) {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
}

func (c *fakeVoiceConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *fakeVoiceConn) ChannelID() *snowflake.ID                               { return &c.channelID }
func (c *fakeVoiceConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return nil }
func (c *fakeVoiceConn) SetOpusFrameProvider(voice.OpusFrameProvider)           {}
func (c *fakeVoiceConn) SetOpusFrameReceiver(voice.OpusFrameReceiver)           {}
func (c *fakeVoiceConn) SetEventHandlerFunc(voice.EventHandlerFunc)             {}

// fakeVoiceManager hands out one conn per bot, as disgo does: CreateConn for a
// guild that already has a conn returns that same conn. Two concurrent raids in
// one guild therefore share the bot's connection — which is what makes one
// raid's teardown reach into the other.
type fakeVoiceManager struct {
	conn    *fakeVoiceConn
	onOpen  func()
	opening sync.Once
}

func (f *fakeVoiceManager) CreateConn(snowflake.ID) voice.Conn {
	if f.onOpen != nil {
		f.onOpen()
	}
	return f.conn
}

func (f *fakeVoiceManager) GetConn(snowflake.ID) voice.Conn { return f.conn }

func (*fakeVoiceManager) HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate)   {}
func (*fakeVoiceManager) HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate) {}
func (*fakeVoiceManager) Conns() iter.Seq[voice.Conn]                            { return func(func(voice.Conn) bool) {} }
func (*fakeVoiceManager) RemoveConn(snowflake.ID)                                {}
func (*fakeVoiceManager) Close(context.Context)                                  {}

// stubRest answers the only REST call the raid path makes. Server deafen is a
// permission the guild may simply not have granted, and the raid is required to
// start anyway, so a failure here is an ordinary path rather than a shortcut.
type stubRest struct {
	rest.Rest
}

func (stubRest) UpdateMember(snowflake.ID, snowflake.ID, discord.MemberUpdate, ...rest.RequestOpt) (*discord.Member, error) {
	return nil, errors.New("missing permissions")
}

// onePool is a PoolService holding a single speaker client.
type onePool struct {
	id     snowflake.ID
	client *bot.Client
}

func (p onePool) GetClientByID(id snowflake.ID) (*bot.Client, bool) {
	if id != p.id {
		return nil, false
	}
	return p.client, true
}
func (p onePool) GetIDs() []snowflake.ID                     { return []snowflake.ID{p.id} }
func (onePool) Reconnect(context.Context, snowflake.ID) bool { return false }
func (onePool) Shutdown(context.Context)                     {}

// meetingPoint releases its waiters once n starts have reached it or given up,
// so a test can hold two goroutines inside the same window instead of hoping
// the scheduler interleaves them. A start that is rejected before it gets
// there counts as having given up — that is what a guarded start looks like,
// and the assertions, not this, are what say whether the guard is right.
type meetingPoint struct {
	mu      sync.Mutex
	n       int
	want    int
	release chan struct{}
}

func newMeetingPoint(want int) *meetingPoint {
	return &meetingPoint{want: want, release: make(chan struct{})}
}

func (m *meetingPoint) count() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	if m.n == m.want {
		close(m.release)
	}
}

// arrive blocks until every other start has arrived or given up.
func (m *meetingPoint) arrive(t *testing.T) {
	t.Helper()
	m.count()
	select {
	case <-m.release:
	case <-time.After(5 * time.Second):
		t.Error("meeting point: the other start neither arrived nor returned")
	}
}

// gaveUp records a start that returned without ever arriving.
func (m *meetingPoint) gaveUp() { m.count() }

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
		ownerClient: &bot.Client{VoiceManager: ownerVM, Caches: cache.New(), Rest: stubRest{}},
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
