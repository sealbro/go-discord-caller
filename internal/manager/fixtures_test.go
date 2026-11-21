package manager

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
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

const (
	testGuildID          = snowflake.ID(8001)
	roleTestGuestGuildID = snowflake.ID(8002)
	testBotID            = snowflake.ID(8100)
	roleTestSpeakerID    = snowflake.ID(500)
	roleTestCallerRoleID = snowflake.ID(700)
	roleTestHostCode     = ally.Code("HOSTCODE")
)

// roleTestChannels gives each guild an owner channel and a speaker channel.
var roleTestChannels = map[snowflake.ID][2]snowflake.ID{
	testGuildID:          {900, 901},
	roleTestGuestGuildID: {910, 911},
}

// await receives from ch, failing the test if nothing arrives in time.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// fakeVoiceConn is a voice.Conn that records closes. Everything the raid setup
// calls on a conn either succeeds or is a no-op; the embedded interface panics
// on anything else, which keeps the fake honest about what the path touches.
type fakeVoiceConn struct {
	voice.Conn
	channelID snowflake.ID

	mu           sync.Mutex
	closes       int
	closeCtxErrs []error
	speakingErr  error
	openErr      error
	open         func(context.Context) error // overrides openErr when set
	onClose      func()
}

func (c *fakeVoiceConn) Open(ctx context.Context, _ snowflake.ID, _ bool, _ bool) error {
	if c.open != nil {
		return c.open(ctx)
	}
	return c.openErr
}

func (c *fakeVoiceConn) Close(ctx context.Context) {
	c.mu.Lock()
	c.closes++
	c.closeCtxErrs = append(c.closeCtxErrs, ctx.Err())
	hook := c.onClose
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (c *fakeVoiceConn) closeContextErrors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.closeCtxErrs...)
}

func (c *fakeVoiceConn) setOnClose(fn func()) {
	c.mu.Lock()
	c.onClose = fn
	c.mu.Unlock()
}

func (c *fakeVoiceConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *fakeVoiceConn) ChannelID() *snowflake.ID                               { return &c.channelID }
func (c *fakeVoiceConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return c.speakingErr }
func (c *fakeVoiceConn) SetOpusFrameProvider(voice.OpusFrameProvider)           {}
func (c *fakeVoiceConn) SetOpusFrameReceiver(voice.OpusFrameReceiver)           {}
func (c *fakeVoiceConn) Gateway() voice.Gateway                                 { return nil }

// fakeVoiceManager hands out one conn per bot, as disgo does: CreateConn for a
// guild that already has a conn returns that same conn. Two concurrent raids in
// one guild therefore share the bot's connection — which is what makes one
// raid's teardown reach into the other. Without a conn it has none to give.
type fakeVoiceManager struct {
	conn   *fakeVoiceConn
	onOpen func()
}

func (f *fakeVoiceManager) CreateConn(snowflake.ID) voice.Conn {
	if f.onOpen != nil {
		f.onOpen()
	}
	return f.GetConn(0)
}

func (f *fakeVoiceManager) GetConn(snowflake.ID) voice.Conn {
	if f.conn == nil {
		return nil
	}
	return f.conn
}

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

// meetingPoint releases its waiters once n callers have reached it or given
// up, so a test can hold several goroutines inside the same window instead of
// hoping the scheduler interleaves them. A caller that is rejected before it
// gets there counts as having given up — that is what a guarded start looks
// like, and the assertions, not this, are what say whether the guard is right.
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

// arrive blocks until every other caller has arrived or given up.
func (m *meetingPoint) arrive(t *testing.T) {
	t.Helper()
	m.count()
	select {
	case <-m.release:
	case <-time.After(5 * time.Second):
		m.mu.Lock()
		defer m.mu.Unlock()
		t.Errorf("meeting point: %d of %d arrived; the others neither arrived nor returned", m.n, m.want)
	}
}

// gaveUp records a caller that returned without ever arriving.
func (m *meetingPoint) gaveUp() { m.count() }

// callerRoleFixture is a Service whose guilds are fully set up except for the
// caller role, with a count of every voice join the raid path attempts.
type callerRoleFixture struct {
	m      *Service
	st     *store.InMemoryStore
	caches cache.Caches
	joins  atomic.Int32

	ownerConn, speakerConn *fakeVoiceConn
	ownerVM, speakerVM     *fakeVoiceManager
}

func newCallerRoleFixture(t *testing.T) *callerRoleFixture {
	t.Helper()
	f := &callerRoleFixture{
		st:     store.NewInMemoryStore(),
		caches: cache.New(cache.WithCaches(cache.FlagsAll)),
	}
	statuses := map[snowflake.ID]*guild.Status{}
	for guildID, channels := range roleTestChannels {
		f.st.BindChannel(guildID, testBotID, channels[0])
		f.st.BindChannel(guildID, roleTestSpeakerID, channels[1])
		for _, channelID := range channels {
			f.caches.AddChannel(voiceChannel(t, guildID, channelID))
		}
		statuses[guildID] = &guild.Status{
			GuildID:     guildID,
			OwnerUserID: testBotID,
			Speakers:    map[snowflake.ID]*guild.Speaker{roleTestSpeakerID: {ID: roleTestSpeakerID, Enabled: true}},
		}
	}

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("manager_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	countJoin := func() { f.joins.Add(1) }
	f.ownerConn, f.speakerConn = &fakeVoiceConn{}, &fakeVoiceConn{}
	f.ownerVM = &fakeVoiceManager{conn: f.ownerConn, onOpen: countJoin}
	f.speakerVM = &fakeVoiceManager{conn: f.speakerConn, onOpen: countJoin}
	f.m = &Service{
		statuses:    statuses,
		store:       f.st,
		poolSvc:     onePool{id: roleTestSpeakerID, client: &bot.Client{VoiceManager: f.speakerVM, Caches: cache.New(), Rest: stubRest{}}},
		ownerClient: &bot.Client{VoiceManager: f.ownerVM, Caches: f.caches, Rest: stubRest{}},
		ownerBotID:  testBotID,
		sessions:    ally.NewManager(),
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	f.m.activeRouters.Store(&empty)
	return f
}

func (f *callerRoleFixture) addMember(guildID, userID snowflake.ID, roleIDs ...snowflake.ID) {
	f.caches.AddMember(discord.Member{GuildID: guildID, User: discord.User{ID: userID}, RoleIDs: roleIDs})
}

func withCallerRole(f *callerRoleFixture) *callerRoleFixture {
	f.st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	f.st.BindRole(roleTestGuestGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	return f
}

func startHostRaid(t *testing.T, f *callerRoleFixture) (context.Context, context.CancelFunc, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
	return ctx, cancel, err
}
