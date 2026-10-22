package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	roleTestGuestGuildID = snowflake.ID(1430511050704289836)
	roleTestSpeakerID    = snowflake.ID(500)
	roleTestCallerRoleID = snowflake.ID(700)
	roleTestHostCode     = ally.Code("HOSTCODE")
)

// roleTestChannels gives each guild an owner channel and a speaker channel.
var roleTestChannels = map[snowflake.ID][2]snowflake.ID{
	testGuildID:          {900, 901},
	roleTestGuestGuildID: {910, 911},
}

// callerRoleFixture is a Service whose guilds are fully set up except for the
// caller role, with a count of every voice join the raid path attempts.
type callerRoleFixture struct {
	m      *Service
	st     *store.InMemoryStore
	caches cache.Caches
	joins  atomic.Int32
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

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("caller_role_required_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	countJoin := func() { f.joins.Add(1) }
	f.m = &Service{
		statuses:    statuses,
		store:       f.st,
		poolSvc:     onePool{id: roleTestSpeakerID, client: &bot.Client{VoiceManager: &fakeVoiceManager{conn: &fakeVoiceConn{}, onOpen: countJoin}, Caches: cache.New(), Rest: stubRest{}}},
		ownerClient: &bot.Client{VoiceManager: &fakeVoiceManager{conn: &fakeVoiceConn{}, onOpen: countJoin}, Caches: f.caches, Rest: stubRest{}},
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

// Without a caller role every non-bot user in the captured channel is relayed —
// to every speaker channel and, through a relay code, into other servers. A
// raid must therefore not start until the role is bound, and must refuse before
// any bot joins voice.
func TestStartVoiceRaidRefusedWithoutCallerRole(t *testing.T) {
	for _, mode := range []guild.RaidMode{guild.RaidModeOneCaller, guild.RaidModeGuildCaller, guild.RaidModeOneManyGuildCaller} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCallerRoleFixture(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, mode)

			if !errors.Is(err, ErrNoCallerRole) {
				t.Errorf("StartVoiceRaid without a caller role: err = %v, want ErrNoCallerRole", err)
			}
			if n := f.joins.Load(); n != 0 {
				t.Errorf("%d bot(s) joined voice for a raid that must be refused", n)
			}
			if f.m.HasActiveSession(testGuildID) {
				t.Error("a raid without a caller role was committed")
			}
		})
	}
}

// A guest that captures relays its own users into the host's server, so it
// needs a caller role just as a host does. The check applies to the effective
// mode: the guest is refused before it joins voice and leaves the ally session.
func TestJoinSessionRefusedWithoutCallerRoleWhenGuestCaptures(t *testing.T) {
	for _, mode := range []guild.RaidMode{guild.RaidModeAllyCaller, guild.RaidModeOneManyAllyCaller} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCallerRoleFixture(t)
			f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, mode, roleTestHostCode)

			if !errors.Is(err, ErrNoCallerRole) {
				t.Errorf("capturing guest without a caller role: err = %v, want ErrNoCallerRole", err)
			}
			if n := f.joins.Load(); n != 0 {
				t.Errorf("%d bot(s) joined voice for a guest join that must be refused", n)
			}
			if _, attached := f.m.sessions.GetByGuild(roleTestGuestGuildID); attached {
				t.Error("refused guest is still attached to the host's ally session")
			}
		})
	}
}

// Guard against over-blocking: a guest that only listens relays nobody, so it
// needs no caller role — including a capturing mode the host downgrades.
func TestJoinSessionAllowsListenerWithoutCallerRole(t *testing.T) {
	cases := []struct {
		name      string
		hostMode  guild.RaidMode
		guestMode guild.RaidMode
	}{
		{"listener", guild.RaidModeGuildCaller, guild.RaidModeAllyListener},
		{"downgraded by host", guild.RaidModeOneCaller, guild.RaidModeAllyCaller},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCallerRoleFixture(t)
			f.m.sessions.Create(roleTestHostCode, testGuildID, tc.hostMode)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			effective, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, tc.guestMode, roleTestHostCode)
			if err != nil {
				t.Fatalf("listen-only guest without a caller role: err = %v, want nil", err)
			}
			if effective.WithCapture() {
				t.Fatalf("effective guest mode %s captures; the case is meant to be listen-only", effective)
			}
		})
	}
}

// The router counts callers through HasCallerRole for the whole raid. With no
// role bound it must count nobody, or unbinding the role mid-raid would turn
// every speaking user into a caller.
func TestHasCallerRoleFalseWhenNoRoleBound(t *testing.T) {
	f := newCallerRoleFixture(t)

	if f.m.HasCallerRole(testGuildID, []snowflake.ID{roleTestCallerRoleID}) {
		t.Error("HasCallerRole with no caller role bound = true; an unbound role must make nobody a caller")
	}
}

func TestAllowFilterWithoutCallerRoleRelaysNobody(t *testing.T) {
	const userID = snowflake.ID(42)
	f := newCallerRoleFixture(t)
	f.addMember(testGuildID, userID)

	if f.m.buildAllowUserFilter(testGuildID).Check(userID) {
		t.Error("allow filter with no caller role relays a member; it must relay nobody")
	}
}

// Guard: with a role bound, only its holders are relayed.
func TestAllowFilterRelaysOnlyCallerRoleHolders(t *testing.T) {
	const (
		callerID    = snowflake.ID(42)
		bystanderID = snowflake.ID(43)
	)
	f := newCallerRoleFixture(t)
	f.st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	f.addMember(testGuildID, callerID, roleTestCallerRoleID)
	f.addMember(testGuildID, bystanderID)

	filter := f.m.buildAllowUserFilter(testGuildID)
	if !filter.Check(callerID) {
		t.Error("member holding the caller role is not relayed")
	}
	if filter.Check(bystanderID) {
		t.Error("member without the caller role is relayed")
	}
}
