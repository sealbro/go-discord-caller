package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// fakeManager implements ManagerService and records calls relevant to the
// handler-under-test. Boolean-returning methods are configured per test via the
// public fields; everything else defaults to a no-op.
type fakeManager struct {
	mu sync.Mutex

	isBotFn         func(discord.User) bool
	hasCallerRoleFn func(snowflake.ID, []snowflake.ID) bool
	hasActiveFn     func(snowflake.ID) bool

	seedCalls           [][]snowflake.ID
	trySeedCalls        []snowflakePair
	removeSpeakerCalls  []snowflakePair
	notifyMemberCalls   []snowflakePair
	updateMixerCalls    []snowflake.ID
	autoRouteCalls      []autoRouteCall
	reconnectCalls      []snowflakePair
	onBotVoiceMoveCalls []botMoveCall
}

type snowflakePair struct{ guild, user snowflake.ID }
type autoRouteCall struct{ guild, channel snowflake.ID }
type botMoveCall struct {
	guild, bot snowflake.ID
	chID       *snowflake.ID
}

func (f *fakeManager) IsBot(u discord.User) bool {
	if f.isBotFn != nil {
		return f.isBotFn(u)
	}
	return u.Bot
}
func (f *fakeManager) HasCallerRole(g snowflake.ID, r []snowflake.ID) bool {
	if f.hasCallerRoleFn != nil {
		return f.hasCallerRoleFn(g, r)
	}
	return false
}
func (f *fakeManager) HasActiveSession(g snowflake.ID) bool {
	if f.hasActiveFn != nil {
		return f.hasActiveFn(g)
	}
	return false
}
func (f *fakeManager) SeedExistingSpeakers(ids []snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dup := make([]snowflake.ID, len(ids))
	copy(dup, ids)
	f.seedCalls = append(f.seedCalls, dup)
}
func (f *fakeManager) TrySeedMember(g, u snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trySeedCalls = append(f.trySeedCalls, snowflakePair{g, u})
}
func (f *fakeManager) RemoveSpeaker(g, u snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeSpeakerCalls = append(f.removeSpeakerCalls, snowflakePair{g, u})
}
func (f *fakeManager) NotifyMemberUpdate(g snowflake.ID, m discord.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyMemberCalls = append(f.notifyMemberCalls, snowflakePair{g, m.User.ID})
}
func (f *fakeManager) UpdateMixerPause(g snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateMixerCalls = append(f.updateMixerCalls, g)
}
func (f *fakeManager) AutoRoute(g, ch snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoRouteCalls = append(f.autoRouteCalls, autoRouteCall{g, ch})
}
func (f *fakeManager) ReconnectBotChannel(_ context.Context, g, b snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconnectCalls = append(f.reconnectCalls, snowflakePair{g, b})
}
func (f *fakeManager) OnBotVoiceMove(_ context.Context, g, b snowflake.ID, ch *snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onBotVoiceMoveCalls = append(f.onBotVoiceMoveCalls, botMoveCall{g, b, ch})
}

// Stubs for the rest of ManagerService — handlers under test do not exercise these.
func (f *fakeManager) StartVoiceRaid(context.Context, snowflake.ID, context.CancelFunc, guild.RaidMode) (ally.Code, error) {
	return "", nil
}
func (f *fakeManager) StopVoiceRaid(context.Context, snowflake.ID) error { return nil }
func (f *fakeManager) JoinSession(context.Context, snowflake.ID, context.CancelFunc, guild.RaidMode, ally.Code) (guild.RaidMode, error) {
	return "", nil
}
func (f *fakeManager) CheckGuildChannelAccess(snowflake.ID) []manager.ChannelAccessWarning {
	return nil
}
func (f *fakeManager) BindRole(snowflake.ID, store.RoleType, snowflake.ID) {}
func (f *fakeManager) UnbindRole(snowflake.ID, store.RoleType)             {}
func (f *fakeManager) BindChannel(snowflake.ID, snowflake.ID, snowflake.ID) {
}
func (f *fakeManager) UnbindChannel(snowflake.ID, snowflake.ID) {}
func (f *fakeManager) GetBoundChannel(snowflake.ID, snowflake.ID) (snowflake.ID, bool) {
	return 0, false
}
func (f *fakeManager) OwnerBotID() snowflake.ID                             { return 0 }
func (f *fakeManager) BindLocale(snowflake.ID, string)                      {}
func (f *fakeManager) UnbindLocale(snowflake.ID)                            {}
func (f *fakeManager) GetLocale(snowflake.ID) string                        { return "" }
func (f *fakeManager) ToggleSpeaker(snowflake.ID, snowflake.ID, bool) error { return nil }
func (f *fakeManager) NextSpeakerID(snowflake.ID) (snowflake.ID, bool)      { return 0, false }
func (f *fakeManager) HasAvailableToken(snowflake.ID) bool                  { return false }
func (f *fakeManager) GetStatus(snowflake.ID) guild.Status                  { return guild.Status{} }
func (f *fakeManager) HasManagerRole(snowflake.ID, []snowflake.ID) bool     { return false }
func (f *fakeManager) StartMetrics()                                        {}
func (f *fakeManager) Shutdown(context.Context)                             {}

// snapshotCalls returns a defensive copy of recorded call slices, holding the
// mutex once to avoid racing with goroutine-spawned handler bodies.
func (f *fakeManager) snapshotCalls() fakeManagerSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeManagerSnapshot{
		seedCalls:           append([][]snowflake.ID(nil), f.seedCalls...),
		trySeedCalls:        append([]snowflakePair(nil), f.trySeedCalls...),
		removeSpeakerCalls:  append([]snowflakePair(nil), f.removeSpeakerCalls...),
		notifyMemberCalls:   append([]snowflakePair(nil), f.notifyMemberCalls...),
		updateMixerCalls:    append([]snowflake.ID(nil), f.updateMixerCalls...),
		reconnectCalls:      append([]snowflakePair(nil), f.reconnectCalls...),
		onBotVoiceMoveCalls: append([]botMoveCall(nil), f.onBotVoiceMoveCalls...),
	}
}

type fakeManagerSnapshot struct {
	seedCalls           [][]snowflake.ID
	trySeedCalls        []snowflakePair
	removeSpeakerCalls  []snowflakePair
	notifyMemberCalls   []snowflakePair
	updateMixerCalls    []snowflake.ID
	reconnectCalls      []snowflakePair
	onBotVoiceMoveCalls []botMoveCall
}

// newTestClient returns a *bot.Client with a working Caches but no network
// state, sufficient for handlers that touch MemberCache.
func newTestClient() *bot.Client {
	return &bot.Client{
		Caches: cache.New(cache.WithCaches(cache.FlagsAll)),
	}
}

// newTestBotMetrics constructs BotMetrics backed by a noop meter so handler
// counters can be incremented without external observability setup.
func newTestBotMetrics(t *testing.T) *telemetry.BotMetrics {
	t.Helper()
	m, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("handlers_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return &m.Bot
}

// waitFor polls until cond returns true or the deadline elapses. Used to
// rendezvous with handler goroutines spawned via `go m.X(...)`.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestOnGuildJoin_SeedsGuild(t *testing.T) {
	t.Parallel()
	f := &fakeManager{}
	h := onGuildJoin(f)

	guildID := snowflake.ID(42)
	h(&events.GuildJoin{
		GenericGuild: &events.GenericGuild{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			GuildID:      guildID,
		},
	})

	waitFor(t, func() bool { return len(f.snapshotCalls().seedCalls) == 1 }, "SeedExistingSpeakers")
	got := f.snapshotCalls().seedCalls[0]
	if len(got) != 1 || got[0] != guildID {
		t.Errorf("SeedExistingSpeakers args: want [%s] got %v", guildID, got)
	}
}

func TestOnGuildMemberAdd_BotOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		isBot     bool
		wantCalls int
	}{
		{"bot user triggers seed", true, 1},
		{"human user is ignored", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeManager{}
			h := onGuildMemberAdd(f)

			guildID := snowflake.ID(10)
			userID := snowflake.ID(20)
			h(&events.GuildMemberJoin{
				GenericGuildMember: &events.GenericGuildMember{
					GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
					GuildID:      guildID,
					Member:       discord.Member{User: discord.User{ID: userID, Bot: tc.isBot}},
				},
			})

			if tc.wantCalls == 0 {
				// Give the goroutine a moment in case the handler is wrong.
				time.Sleep(20 * time.Millisecond)
				if got := len(f.snapshotCalls().trySeedCalls); got != 0 {
					t.Errorf("expected no TrySeedMember calls; got %d", got)
				}
				return
			}
			waitFor(t, func() bool { return len(f.snapshotCalls().trySeedCalls) == 1 }, "TrySeedMember")
			got := f.snapshotCalls().trySeedCalls[0]
			if got.guild != guildID || got.user != userID {
				t.Errorf("TrySeedMember args: want (%s,%s) got (%s,%s)", guildID, userID, got.guild, got.user)
			}
		})
	}
}

func TestOnGuildMemberLeave_BotOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		isBot     bool
		wantCalls int
	}{
		{"bot user triggers remove", true, 1},
		{"human user is ignored", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeManager{}
			h := onGuildMemberLeave(f)

			guildID := snowflake.ID(10)
			userID := snowflake.ID(20)
			h(&events.GuildMemberLeave{
				GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
				GuildID:      guildID,
				User:         discord.User{ID: userID, Bot: tc.isBot},
				Member:       discord.Member{User: discord.User{ID: userID, Bot: tc.isBot}},
			})

			if tc.wantCalls == 0 {
				time.Sleep(20 * time.Millisecond)
				if got := len(f.snapshotCalls().removeSpeakerCalls); got != 0 {
					t.Errorf("expected no RemoveSpeaker calls; got %d", got)
				}
				return
			}
			waitFor(t, func() bool { return len(f.snapshotCalls().removeSpeakerCalls) == 1 }, "RemoveSpeaker")
			got := f.snapshotCalls().removeSpeakerCalls[0]
			if got.guild != guildID || got.user != userID {
				t.Errorf("RemoveSpeaker args: want (%s,%s) got (%s,%s)", guildID, userID, got.guild, got.user)
			}
		})
	}
}

func TestOnVoiceLeave_BotReconnectsWhenSessionActive(t *testing.T) {
	t.Parallel()
	f := &fakeManager{hasActiveFn: func(snowflake.ID) bool { return true }}
	h := onVoiceLeave(f, newTestBotMetrics(t))

	guildID := snowflake.ID(50)
	botID := snowflake.ID(99)
	chID := snowflake.ID(1001)
	h(&events.GuildVoiceLeave{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: guildID, UserID: botID, ChannelID: &chID},
			Member:       discord.Member{User: discord.User{ID: botID, Bot: true}},
		},
		OldVoiceState: discord.VoiceState{GuildID: guildID, UserID: botID, ChannelID: &chID},
	})
	waitFor(t, func() bool { return len(f.snapshotCalls().reconnectCalls) == 1 }, "ReconnectBotChannel")
	snap := f.snapshotCalls()
	got := snap.reconnectCalls[0]
	if got.guild != guildID || got.user != botID {
		t.Errorf("ReconnectBotChannel args: want (%s,%s) got (%s,%s)", guildID, botID, got.guild, got.user)
	}
	// Bots must not trigger UpdateMixerPause / VoiceCallerAdd on leave.
	if len(snap.updateMixerCalls) != 0 {
		t.Errorf("UpdateMixerPause must not be called for bot leave; got %d calls", len(snap.updateMixerCalls))
	}
}

func TestOnVoiceLeave_BotIgnoredWithoutSession(t *testing.T) {
	t.Parallel()
	f := &fakeManager{} // HasActiveSession defaults to false
	h := onVoiceLeave(f, newTestBotMetrics(t))

	chID := snowflake.ID(1001)
	h(&events.GuildVoiceLeave{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: 50, UserID: 99, ChannelID: &chID},
			Member:       discord.Member{User: discord.User{ID: 99, Bot: true}},
		},
		OldVoiceState: discord.VoiceState{GuildID: 50, UserID: 99, ChannelID: &chID},
	})
	// Give the goroutine a moment in case the handler spawns one anyway.
	time.Sleep(20 * time.Millisecond)
	if got := len(f.snapshotCalls().reconnectCalls); got != 0 {
		t.Errorf("ReconnectBotChannel must not be called when HasActiveSession=false; got %d", got)
	}
}

func TestOnVoiceLeave_HumanUpdatesMixerAndCallerCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		hasCallerRole     bool
		wantCallerCounted bool
	}{
		{"caller leaves", true, true},
		{"non-caller leaves", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeManager{
				hasCallerRoleFn: func(snowflake.ID, []snowflake.ID) bool { return tc.hasCallerRole },
			}
			h := onVoiceLeave(f, newTestBotMetrics(t))

			guildID := snowflake.ID(50)
			userID := snowflake.ID(123)
			oldCh := snowflake.ID(1001)
			h(&events.GuildVoiceLeave{
				GenericGuildVoiceState: &events.GenericGuildVoiceState{
					GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
					VoiceState:   discord.VoiceState{GuildID: guildID, UserID: userID, ChannelID: nil},
					Member:       discord.Member{User: discord.User{ID: userID}, RoleIDs: []snowflake.ID{1}},
				},
				OldVoiceState: discord.VoiceState{GuildID: guildID, UserID: userID, ChannelID: &oldCh},
			})

			snap := f.snapshotCalls()
			if len(snap.updateMixerCalls) != 1 || snap.updateMixerCalls[0] != guildID {
				t.Errorf("UpdateMixerPause: want [%s] got %v", guildID, snap.updateMixerCalls)
			}
			// We rely on the BotMetrics noop meter to not panic when VoiceCallerAdd
			// is invoked; the call itself is unobservable without an OTel collector.
			// The handler must not invoke any bot-only paths here.
			if len(snap.reconnectCalls) != 0 {
				t.Errorf("ReconnectBotChannel must not be called for human leave; got %d", len(snap.reconnectCalls))
			}
		})
	}
}

func TestOnVoiceMove_BotDelegatesToManager(t *testing.T) {
	t.Parallel()
	f := &fakeManager{}
	h := onVoiceMove(f)

	guildID := snowflake.ID(50)
	botID := snowflake.ID(99)
	newCh := snowflake.ID(2002)
	h(&events.GuildVoiceMove{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: guildID, UserID: botID, ChannelID: &newCh},
			Member:       discord.Member{User: discord.User{ID: botID, Bot: true}},
		},
	})

	snap := f.snapshotCalls()
	if len(snap.onBotVoiceMoveCalls) != 1 {
		t.Fatalf("OnBotVoiceMove: want 1 call got %d", len(snap.onBotVoiceMoveCalls))
	}
	got := snap.onBotVoiceMoveCalls[0]
	if got.guild != guildID || got.bot != botID {
		t.Errorf("OnBotVoiceMove ids: want (%s,%s) got (%s,%s)", guildID, botID, got.guild, got.bot)
	}
	if got.chID == nil || *got.chID != newCh {
		t.Errorf("OnBotVoiceMove channel: want %s got %v", newCh, got.chID)
	}
	if len(snap.updateMixerCalls) != 0 {
		t.Errorf("UpdateMixerPause must not be called for bot move; got %d calls", len(snap.updateMixerCalls))
	}
}

func TestOnVoiceMove_HumanUpdatesMixerPause(t *testing.T) {
	t.Parallel()
	f := &fakeManager{}
	h := onVoiceMove(f)

	guildID := snowflake.ID(50)
	newCh := snowflake.ID(2002)
	h(&events.GuildVoiceMove{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: guildID, UserID: 123, ChannelID: &newCh},
			Member:       discord.Member{User: discord.User{ID: 123, Bot: false}},
		},
	})
	snap := f.snapshotCalls()
	if len(snap.updateMixerCalls) != 1 || snap.updateMixerCalls[0] != guildID {
		t.Errorf("UpdateMixerPause: want [%s] got %v", guildID, snap.updateMixerCalls)
	}
	if len(snap.onBotVoiceMoveCalls) != 0 {
		t.Errorf("OnBotVoiceMove must not be called for human move; got %d", len(snap.onBotVoiceMoveCalls))
	}
}

func TestOnVoiceJoin_HumanUpdatesPauseAndNotifies(t *testing.T) {
	t.Parallel()
	f := &fakeManager{
		hasCallerRoleFn: func(snowflake.ID, []snowflake.ID) bool { return true },
	}
	h := onVoiceJoin(f, newTestBotMetrics(t))

	guildID := snowflake.ID(50)
	userID := snowflake.ID(123)
	chID := snowflake.ID(1001)
	h(&events.GuildVoiceJoin{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: guildID, UserID: userID, ChannelID: &chID},
			Member: discord.Member{
				User:    discord.User{ID: userID, Bot: false},
				RoleIDs: []snowflake.ID{42},
			},
		},
	})

	snap := f.snapshotCalls()
	if len(snap.notifyMemberCalls) != 1 || snap.notifyMemberCalls[0].user != userID {
		t.Errorf("NotifyMemberUpdate: want one call for %s got %v", userID, snap.notifyMemberCalls)
	}
	if len(snap.updateMixerCalls) != 1 || snap.updateMixerCalls[0] != guildID {
		t.Errorf("UpdateMixerPause: want [%s] got %v", guildID, snap.updateMixerCalls)
	}
}

func TestOnVoiceJoin_BotIsSkipped(t *testing.T) {
	t.Parallel()
	f := &fakeManager{
		isBotFn: func(u discord.User) bool { return u.Bot },
	}
	h := onVoiceJoin(f, newTestBotMetrics(t))

	chID := snowflake.ID(1001)
	h(&events.GuildVoiceJoin{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			GenericEvent: events.NewGenericEvent(newTestClient(), 0, 0),
			VoiceState:   discord.VoiceState{GuildID: 50, UserID: 99, ChannelID: &chID},
			Member:       discord.Member{User: discord.User{ID: 99, Bot: true}},
		},
	})
	snap := f.snapshotCalls()
	if len(snap.notifyMemberCalls) != 0 || len(snap.updateMixerCalls) != 0 {
		t.Errorf("bot join must short-circuit: notify=%d updateMixer=%d",
			len(snap.notifyMemberCalls), len(snap.updateMixerCalls))
	}
}
