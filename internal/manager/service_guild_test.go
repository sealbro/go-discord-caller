package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

const (
	speakerOne   snowflake.ID = 601
	speakerTwo   snowflake.ID = 602
	speakerThree snowflake.ID = 603
	// speakerNoClient is listed by the pool but has no connected client.
	speakerNoClient snowflake.ID = 604
)

// speakerPool is a PoolService whose speaker clients know their own user, as a
// connected disgo client does after Ready.
type speakerPool struct {
	ids       []snowflake.ID
	clients   map[snowflake.ID]*bot.Client
	shutdowns atomic.Int32
}

func newSpeakerPool(ids ...snowflake.ID) *speakerPool {
	p := &speakerPool{ids: ids, clients: make(map[snowflake.ID]*bot.Client)}
	for _, id := range ids {
		if id == speakerNoClient {
			continue
		}
		caches := cache.New()
		caches.SetSelfUser(discord.OAuth2User{User: discord.User{ID: id, Username: fmt.Sprintf("speaker-%d", id), Bot: true}})
		p.clients[id] = &bot.Client{Caches: caches}
	}
	return p
}

func (p *speakerPool) GetClientByID(id snowflake.ID) (*bot.Client, bool) {
	c, ok := p.clients[id]
	return c, ok
}
func (p *speakerPool) GetIDs() []snowflake.ID                     { return p.ids }
func (*speakerPool) Reconnect(context.Context, snowflake.ID) bool { return false }
func (p *speakerPool) Shutdown(context.Context)                   { p.shutdowns.Add(1) }

func (p *speakerPool) rename(id snowflake.ID, username string) {
	p.clients[id].Caches.SetSelfUser(discord.OAuth2User{User: discord.User{ID: id, Username: username, Bot: true}})
}

// membersRest answers member lookups from a fixed guild roster and counts them.
type membersRest struct {
	rest.Rest
	members map[snowflake.ID]bool

	mu      sync.Mutex
	lookups int
}

func (r *membersRest) GetMember(guildID, userID snowflake.ID, _ ...rest.RequestOpt) (*discord.Member, error) {
	r.mu.Lock()
	r.lookups++
	r.mu.Unlock()
	if !r.members[userID] {
		return nil, errors.New("unknown member")
	}
	return &discord.Member{GuildID: guildID, User: discord.User{ID: userID, Bot: true}}, nil
}

func (*membersRest) GetGuildChannels(snowflake.ID, ...rest.RequestOpt) ([]discord.GuildChannel, error) {
	return nil, errors.New("channels unavailable")
}

func (r *membersRest) lookupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups
}

func newGuildService(pool *speakerPool, rst rest.Rest) (*Service, store.Store) {
	st := store.NewInMemoryStore()
	owner := &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll)), Rest: rst}
	return NewService(st, pool, owner, testBotID, config.TestConfig{}, nil), st
}

func speakerIDs(st guild.Status) []snowflake.ID {
	ids := make([]snowflake.ID, 0, len(st.Speakers))
	for id := range st.Speakers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestEnsureGuildReadyRegistersPoolBotsAlreadyInTheGuild(t *testing.T) {
	t.Parallel()
	pool := newSpeakerPool(speakerOne, speakerTwo, speakerThree, speakerNoClient)
	rst := &membersRest{members: map[snowflake.ID]bool{speakerOne: true, speakerThree: true, speakerNoClient: true}}
	m, st := newGuildService(pool, rst)
	st.SetSpeakerEnabled(testGuildID, speakerThree, false)

	m.EnsureGuildReady(testGuildID)

	status := m.GetStatus(testGuildID)
	if got, want := speakerIDs(status), []snowflake.ID{speakerOne, speakerThree}; !slices.Equal(got, want) {
		t.Fatalf("registered speakers = %v, want %v", got, want)
	}
	if !status.Speakers[speakerOne].Enabled || status.Speakers[speakerThree].Enabled {
		t.Errorf("enabled state not taken from the store: one=%v three=%v",
			status.Speakers[speakerOne].Enabled, status.Speakers[speakerThree].Enabled)
	}
	if got := status.Speakers[speakerOne].Username; got != "speaker-601" {
		t.Errorf("username = %q, want the speaker client's own user", got)
	}
	for _, id := range []snowflake.ID{speakerOne, speakerThree} {
		if _, ok := m.ownerClient.Caches.Member(testGuildID, id); !ok {
			t.Errorf("member %s not warmed into the owner cache", id)
		}
	}

	before := rst.lookupCount()
	m.EnsureGuildReady(testGuildID)
	if after := rst.lookupCount(); after != before {
		t.Errorf("an already seeded guild was seeded again: %d more member lookups", after-before)
	}
}

func TestSeedExistingSpeakersKeepsRegisteredSpeakers(t *testing.T) {
	t.Parallel()
	pool := newSpeakerPool(speakerOne)
	m, _ := newGuildService(pool, &membersRest{members: map[snowflake.ID]bool{speakerOne: true}})

	m.SeedExistingSpeakers([]snowflake.ID{testGuildID})
	if err := m.ToggleSpeaker(testGuildID, speakerOne, false); err != nil {
		t.Fatalf("ToggleSpeaker: %v", err)
	}
	m.SeedExistingSpeakers([]snowflake.ID{testGuildID})

	if sp := m.GetStatus(testGuildID).Speakers[speakerOne]; sp == nil || sp.Enabled {
		t.Errorf("reseeding replaced the registered speaker: %+v", sp)
	}
}

func TestNextSpeakerIDSkipsBotsAlreadyInTheGuild(t *testing.T) {
	t.Parallel()
	pool := newSpeakerPool(speakerOne, speakerTwo, speakerThree)
	rst := &membersRest{members: map[snowflake.ID]bool{speakerOne: true}}
	m, _ := newGuildService(pool, rst)
	m.EnsureGuildReady(testGuildID)
	// Joined Discord-side but not yet registered, e.g. while its member-join
	// event is still in flight.
	m.ownerClient.Caches.AddMember(discord.Member{GuildID: testGuildID, User: discord.User{ID: speakerTwo}})

	if got, ok := m.NextSpeakerID(testGuildID); !ok || got != speakerThree {
		t.Errorf("NextSpeakerID = %s, %v; want %s, true", got, ok, speakerThree)
	}
	if !m.HasAvailableToken(testGuildID) {
		t.Error("HasAvailableToken = false with an uninvited bot left")
	}

	rst.members[speakerThree] = true
	if got, ok := m.NextSpeakerID(testGuildID); ok {
		t.Errorf("NextSpeakerID = %s with every bot in the guild, want none", got)
	}
	if m.HasAvailableToken(testGuildID) {
		t.Error("HasAvailableToken = true with every bot in the guild")
	}
}

func TestTrySeedMember(t *testing.T) {
	t.Parallel()
	const stranger snowflake.ID = 999

	t.Run("ignores a guild that was never seeded", func(t *testing.T) {
		t.Parallel()
		m, _ := newGuildService(newSpeakerPool(speakerOne), &membersRest{})
		m.TrySeedMember(testGuildID, speakerOne)
		if m.HasActiveSession(testGuildID) || len(m.statuses) != 0 {
			t.Errorf("statuses = %v, want none", m.statuses)
		}
	})
	t.Run("ignores users outside the pool", func(t *testing.T) {
		t.Parallel()
		m, _ := newGuildService(newSpeakerPool(speakerOne), &membersRest{})
		m.EnsureGuildReady(testGuildID)
		m.TrySeedMember(testGuildID, stranger)
		if got := speakerIDs(m.GetStatus(testGuildID)); len(got) != 0 {
			t.Errorf("speakers = %v, want none", got)
		}
	})
	t.Run("registers a pool bot that joined", func(t *testing.T) {
		t.Parallel()
		m, st := newGuildService(newSpeakerPool(speakerOne), &membersRest{})
		m.EnsureGuildReady(testGuildID)
		st.SetSpeakerEnabled(testGuildID, speakerOne, false)

		m.TrySeedMember(testGuildID, speakerOne)

		sp := m.GetStatus(testGuildID).Speakers[speakerOne]
		if sp == nil || sp.Enabled {
			t.Errorf("speaker = %+v, want registered and disabled as stored", sp)
		}
	})
	t.Run("refreshes the username and keeps the enabled state", func(t *testing.T) {
		t.Parallel()
		pool := newSpeakerPool(speakerOne)
		m, _ := newGuildService(pool, &membersRest{members: map[snowflake.ID]bool{speakerOne: true}})
		m.EnsureGuildReady(testGuildID)
		if err := m.ToggleSpeaker(testGuildID, speakerOne, false); err != nil {
			t.Fatalf("ToggleSpeaker: %v", err)
		}
		pool.rename(speakerOne, "renamed")

		m.TrySeedMember(testGuildID, speakerOne)

		sp := m.GetStatus(testGuildID).Speakers[speakerOne]
		if sp.Username != "renamed" || sp.Enabled {
			t.Errorf("speaker = %+v, want username refreshed and still disabled", sp)
		}
	})
}

func TestRemoveSpeaker(t *testing.T) {
	t.Parallel()
	m, _ := newGuildService(newSpeakerPool(speakerOne, speakerTwo),
		&membersRest{members: map[snowflake.ID]bool{speakerOne: true, speakerTwo: true}})
	m.RemoveSpeaker(testGuildID, speakerOne) // unseeded guild: nothing to do
	m.EnsureGuildReady(testGuildID)

	m.RemoveSpeaker(testGuildID, speakerOne)

	if got := speakerIDs(m.GetStatus(testGuildID)); !slices.Equal(got, []snowflake.ID{speakerTwo}) {
		t.Errorf("speakers = %v, want [%s]", got, speakerTwo)
	}
}

func TestToggleSpeakerRejectsUnknownGuildOrSpeaker(t *testing.T) {
	t.Parallel()
	m, st := newGuildService(newSpeakerPool(speakerOne), &membersRest{members: map[snowflake.ID]bool{speakerOne: true}})

	if err := m.ToggleSpeaker(testGuildID, speakerOne, false); err == nil {
		t.Error("ToggleSpeaker succeeded for a guild that was never seeded")
	}
	m.EnsureGuildReady(testGuildID)
	if err := m.ToggleSpeaker(testGuildID, speakerTwo, false); err == nil {
		t.Error("ToggleSpeaker succeeded for a speaker not in the guild")
	}
	if !st.IsSpeakerEnabled(testGuildID, speakerTwo) {
		t.Error("a rejected toggle was persisted")
	}
}

func TestGetStatusOfUnknownGuildIsEmpty(t *testing.T) {
	t.Parallel()
	m, _ := newGuildService(newSpeakerPool(), &membersRest{})
	st := m.GetStatus(testGuildID)
	if st.GuildID != testGuildID || st.OwnerUserID != testBotID || len(st.Speakers) != 0 || st.Session != nil {
		t.Errorf("status = %+v, want an empty status for the guild", st)
	}
}

func TestGetStatusEnrichesFromStoreAndCache(t *testing.T) {
	t.Parallel()
	const (
		callerRole   snowflake.ID = 11
		managerRole  snowflake.ID = 12
		ownerChannel snowflake.ID = 21
		speakerChan  snowflake.ID = 22
	)
	m, st := newGuildService(newSpeakerPool(speakerOne, speakerTwo),
		&membersRest{members: map[snowflake.ID]bool{speakerOne: true, speakerTwo: true}})
	m.EnsureGuildReady(testGuildID)
	st.BindRole(testGuildID, store.RoleTypeCaller, callerRole)
	st.BindRole(testGuildID, store.RoleTypeManager, managerRole)
	st.BindChannel(testGuildID, testBotID, ownerChannel)
	st.BindChannel(testGuildID, speakerOne, speakerChan)
	code := st.GetOrCreateAllyCode(testGuildID)
	m.ownerClient.Caches.AddGuild(discord.CacheGuild{Guild: discord.Guild{ID: testGuildID, Name: "Raid HQ"}})

	got := m.GetStatus(testGuildID)

	if got.CallerRoleID == nil || *got.CallerRoleID != callerRole {
		t.Errorf("CallerRoleID = %v, want %s", got.CallerRoleID, callerRole)
	}
	if got.ManagerRoleID == nil || *got.ManagerRoleID != managerRole {
		t.Errorf("ManagerRoleID = %v, want %s", got.ManagerRoleID, managerRole)
	}
	wantChannels := map[snowflake.ID]snowflake.ID{testBotID: ownerChannel, speakerOne: speakerChan}
	if fmt.Sprint(got.BoundChannels) != fmt.Sprint(wantChannels) {
		t.Errorf("BoundChannels = %v, want %v", got.BoundChannels, wantChannels)
	}
	if got.AllyCode != code {
		t.Errorf("AllyCode = %q, want %q", got.AllyCode, code)
	}
	if got.GuildName != "Raid HQ" {
		t.Errorf("GuildName = %q, want the cached guild name", got.GuildName)
	}
}

func TestGetStatusIsADeepCopy(t *testing.T) {
	t.Parallel()
	m, _ := newGuildService(newSpeakerPool(speakerOne), &membersRest{members: map[snowflake.ID]bool{speakerOne: true}})
	m.EnsureGuildReady(testGuildID)
	m.statuses[testGuildID].Session = &guild.Session{
		Speakers: []guild.Speaker{{ID: speakerOne, Username: "live"}},
		Cancel:   func() {},
		Cleanup:  func() {},
	}

	snap := m.GetStatus(testGuildID)
	if snap.Session.Cancel != nil || snap.Session.Cleanup != nil {
		t.Error("snapshot exposes the session's Cancel/Cleanup")
	}
	snap.Speakers[speakerOne].Enabled = false
	snap.Session.Speakers[0].Username = "changed"
	snap.BoundChannels[speakerOne] = 1

	again := m.GetStatus(testGuildID)
	if !again.Speakers[speakerOne].Enabled || again.Session.Speakers[0].Username != "live" || len(again.BoundChannels) != 0 {
		t.Errorf("mutating a snapshot changed the service state: %+v", again)
	}
}

func TestGetStatusNamesRelayPeers(t *testing.T) {
	t.Parallel()
	const (
		host         snowflake.ID = 71
		namedGuest   snowflake.ID = 72
		unnamedGuest snowflake.ID = 73
	)
	m, _ := newGuildService(newSpeakerPool(), &membersRest{})
	for _, g := range []snowflake.ID{host, namedGuest, unnamedGuest} {
		m.EnsureGuildReady(g)
	}
	m.ownerClient.Caches.AddGuild(discord.CacheGuild{Guild: discord.Guild{ID: host, Name: "Host"}})
	m.ownerClient.Caches.AddGuild(discord.CacheGuild{Guild: discord.Guild{ID: namedGuest, Name: "Guest"}})

	sess := m.sessions.Create(ally.Code("RELAY234"), host, guild.RaidModeOneCaller)
	for _, g := range []snowflake.ID{namedGuest, unnamedGuest} {
		if _, err := m.sessions.Join(sess.Code, g); err != nil {
			t.Fatalf("Join: %v", err)
		}
		sess.AddGuild(g, nil)
	}
	m.statuses[host].Session = &guild.Session{}
	m.statuses[namedGuest].Session = &guild.Session{IsGuest: true}

	names := m.GetStatus(host).GuestGuildNames
	slices.Sort(names)
	if want := []string{unnamedGuest.String(), "Guest"}; !slices.Equal(names, want) {
		t.Errorf("GuestGuildNames = %v, want %v (an uncached guest falls back to its ID)", names, want)
	}
	if got := m.GetStatus(namedGuest).HostGuildName; got != "Host" {
		t.Errorf("HostGuildName = %q, want Host", got)
	}
	if got := m.GetStatus(unnamedGuest); got.HostGuildName != "" || got.GuestGuildNames != nil {
		t.Errorf("guild without a session got relay info: %+v", got)
	}
}

const connectDenied = discord.PermissionConnect

// voiceChannelDenying returns a voice channel in which deniedTo lacks Connect.
func voiceChannelDenying(t *testing.T, channelID snowflake.ID, deniedTo ...snowflake.ID) discord.GuildVoiceChannel {
	t.Helper()
	overwrites := make([]map[string]any, 0, len(deniedTo))
	for _, id := range deniedTo {
		overwrites = append(overwrites, map[string]any{
			"id": id.String(), "type": 1, "allow": "0", "deny": fmt.Sprint(int64(connectDenied)),
		})
	}
	raw, err := json.Marshal(map[string]any{
		"id": channelID.String(), "guild_id": testGuildID.String(), "type": 2, "name": "voice",
		"permission_overwrites": overwrites,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ch discord.GuildVoiceChannel
	if err := json.Unmarshal(raw, &ch); err != nil {
		t.Fatalf("unmarshal voice channel: %v", err)
	}
	return ch
}

func TestCheckGuildChannelAccess(t *testing.T) {
	t.Parallel()
	const (
		openChannel     snowflake.ID = 31
		lockedChannel   snowflake.ID = 32
		uncachedChannel snowflake.ID = 33
		speakerFour     snowflake.ID = 605
		speakerFive     snowflake.ID = 606
	)
	ids := []snowflake.ID{speakerOne, speakerTwo, speakerThree, speakerFour, speakerFive}
	members := map[snowflake.ID]bool{}
	for _, id := range ids {
		members[id] = true
	}

	setup := func(t *testing.T, test config.TestConfig) *Service {
		m, st := newGuildService(newSpeakerPool(ids...), &membersRest{members: members})
		m.test = test
		m.EnsureGuildReady(testGuildID)

		caches := m.ownerClient.Caches
		caches.AddRole(discord.Role{ID: testGuildID, GuildID: testGuildID, Permissions: ChannelAccessPermissions})
		caches.AddChannel(voiceChannelDenying(t, openChannel))
		caches.AddChannel(voiceChannelDenying(t, lockedChannel, testBotID, speakerOne, speakerTwo, speakerFive))
		caches.AddMember(discord.Member{GuildID: testGuildID, User: discord.User{ID: testBotID}})
		// speakerFive is a guild member the owner cache has not seen.
		caches.RemoveMember(testGuildID, speakerFive)

		st.BindChannel(testGuildID, testBotID, lockedChannel)
		st.BindChannel(testGuildID, speakerOne, lockedChannel)    // denied
		st.BindChannel(testGuildID, speakerTwo, lockedChannel)    // denied but disabled
		st.BindChannel(testGuildID, speakerThree, openChannel)    // fine
		st.BindChannel(testGuildID, speakerFour, uncachedChannel) // unknown channel
		st.BindChannel(testGuildID, speakerFive, lockedChannel)   // unknown member
		if err := m.ToggleSpeaker(testGuildID, speakerTwo, false); err != nil {
			t.Fatalf("ToggleSpeaker: %v", err)
		}
		return m
	}

	t.Run("reports only bots definitely denied", func(t *testing.T) {
		t.Parallel()
		got := setup(t, config.TestConfig{}).CheckGuildChannelAccess(testGuildID)
		slices.SortFunc(got, func(a, b ChannelAccessWarning) int { return int(a.BotID - b.BotID) })
		want := []ChannelAccessWarning{
			{BotID: speakerOne, ChannelID: lockedChannel},
			{BotID: testBotID, ChannelID: lockedChannel},
		}
		slices.SortFunc(want, func(a, b ChannelAccessWarning) int { return int(a.BotID - b.BotID) })
		if !slices.Equal(got, want) {
			t.Errorf("warnings = %v, want %v", got, want)
		}
	})
	t.Run("skipped for test bot accounts", func(t *testing.T) {
		t.Parallel()
		if got := setup(t, config.TestConfig{AllowBots: true}).CheckGuildChannelAccess(testGuildID); got != nil {
			t.Errorf("warnings = %v, want none in test mode", got)
		}
	})
	t.Run("unseeded guild", func(t *testing.T) {
		t.Parallel()
		m, _ := newGuildService(newSpeakerPool(), &membersRest{})
		if got := m.CheckGuildChannelAccess(testGuildID); got != nil {
			t.Errorf("warnings = %v, want none", got)
		}
	})
}

func TestShutdownStopsEveryActiveRaidThenThePool(t *testing.T) {
	t.Parallel()
	const (
		hostGuild  snowflake.ID = 81
		guestGuild snowflake.ID = 82
		idleGuild  snowflake.ID = 83
	)
	pool := newSpeakerPool()
	m, _ := newGuildService(pool, &membersRest{})
	conn := &fakeVoiceConn{}
	m.ownerClient.VoiceManager = &fakeVoiceManager{conn: conn}
	for _, g := range []snowflake.ID{hostGuild, guestGuild, idleGuild} {
		m.EnsureGuildReady(g)
	}

	var cancels, cleanups atomic.Int32
	session := func(isGuest bool) *guild.Session {
		return &guild.Session{
			IsGuest: isGuest,
			Cancel:  func() { cancels.Add(1) },
			Cleanup: func() { cleanups.Add(1) },
		}
	}
	m.statuses[hostGuild].Session = session(false)
	m.statuses[guestGuild].Session = session(true)
	m.sessions.Create(ally.Code("HOSTCODE"), hostGuild, guild.RaidModeOneCaller)

	m.Shutdown(context.Background())

	if cancels.Load() != 2 || cleanups.Load() != 2 {
		t.Errorf("cancelled %d and cleaned up %d sessions, want 2 each", cancels.Load(), cleanups.Load())
	}
	for _, g := range []snowflake.ID{hostGuild, guestGuild} {
		if m.HasActiveSession(g) {
			t.Errorf("guild %s still has a session after shutdown", g)
		}
	}
	if conn.closeCount() != 1 {
		t.Errorf("owner voice conn closed %d times, want once for the host raid", conn.closeCount())
	}
	if _, ok := m.sessions.GetByGuild(hostGuild); ok {
		t.Error("host relay session still registered after shutdown")
	}
	if pool.shutdowns.Load() != 1 {
		t.Errorf("pool shut down %d times, want 1", pool.shutdowns.Load())
	}
}
