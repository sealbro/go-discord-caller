package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// channelsRest answers the guild-channel fetch warmGuildCache makes.
type channelsRest struct {
	rest.Rest
	channels []discord.GuildChannel
	err      error
}

func (r channelsRest) GetGuildChannels(snowflake.ID, ...rest.RequestOpt) ([]discord.GuildChannel, error) {
	return r.channels, r.err
}

func (channelsRest) GetMember(snowflake.ID, snowflake.ID, ...rest.RequestOpt) (*discord.Member, error) {
	return nil, errors.New("not a member")
}

const (
	bindingsSpeakerID = snowflake.ID(500)
	keptChannel       = snowflake.ID(900)
	deletedChannel    = snowflake.ID(901)
)

func bindingsService(st store.Store, ownerClient *bot.Client) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       st,
		poolSvc:     onePool{id: bindingsSpeakerID, client: &bot.Client{Caches: cache.New()}},
		ownerClient: ownerClient,
		ownerBotID:  testBotID,
		reconnect:   newReconnectState(),
	}
}

func assertBound(t *testing.T, st store.Store, botID, want snowflake.ID) {
	t.Helper()
	got, ok := st.GetBoundChannel(testGuildID, botID)
	switch {
	case want == 0 && ok:
		t.Errorf("bot %s still bound to %s", botID, got)
	case want != 0 && (!ok || got != want):
		t.Errorf("bot %s bound to %s (%v), want %s", botID, got, ok, want)
	}
}

func TestChannelDeleted_UnbindsOnlyBotsBoundToIt(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, deletedChannel)
	st.BindChannel(testGuildID, bindingsSpeakerID, keptChannel)

	bindingsService(st, &bot.Client{Caches: cache.New()}).ChannelDeleted(testGuildID, deletedChannel)

	assertBound(t, st, testBotID, 0)
	assertBound(t, st, bindingsSpeakerID, keptChannel)
}

// A channel deleted while the process was down produces no delete event, so the
// channel list fetched at startup is the only signal left.
func TestWarmGuildCache_UnbindsChannelsDeletedWhileOffline(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, keptChannel)
	st.BindChannel(testGuildID, bindingsSpeakerID, deletedChannel)

	ownerRest := channelsRest{channels: []discord.GuildChannel{voiceChannel(t, testGuildID, keptChannel)}}
	ownerClient := &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll)), Rest: ownerRest}
	bindingsService(st, ownerClient).warmGuildCache(testGuildID)

	assertBound(t, st, testBotID, keptChannel)
	assertBound(t, st, bindingsSpeakerID, 0)
}

func TestWarmGuildCache_KeepsBindingsWhenChannelFetchFails(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, bindingsSpeakerID, keptChannel)

	ownerRest := channelsRest{err: errors.New("context deadline exceeded")}
	ownerClient := &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll)), Rest: ownerRest}
	bindingsService(st, ownerClient).warmGuildCache(testGuildID)

	assertBound(t, st, bindingsSpeakerID, keptChannel)
}

// Without a channel the owner bot cannot carry the raid, and before this check
// the failure only surfaced after every speaker had joined.
func TestStartVoiceRaid_FailsBeforeSpeakersJoinWhenOwnerChannelIsGone(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, deletedChannel)
	st.BindChannel(testGuildID, bindingsSpeakerID, keptChannel)

	var joined atomic.Bool
	speakerVM := &fakeVoiceManager{conn: &fakeVoiceConn{channelID: keptChannel}, onOpen: func() { joined.Store(true) }}
	m := bindingsService(st, &bot.Client{Caches: cachesWithChannels(t, testGuildID, keptChannel)})
	m.statuses[testGuildID] = &guild.Status{
		GuildID:     testGuildID,
		OwnerUserID: testBotID,
		Speakers:    map[snowflake.ID]*guild.Speaker{bindingsSpeakerID: {ID: bindingsSpeakerID, Enabled: true}},
	}
	m.poolSvc = onePool{id: bindingsSpeakerID, client: &bot.Client{VoiceManager: speakerVM, Caches: cache.New(), Rest: stubRest{}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)

	if !errors.Is(err, ErrNoOwnerChannel) {
		t.Fatalf("want ErrNoOwnerChannel, got %v", err)
	}
	if joined.Load() {
		t.Error("a speaker joined although the raid could never start")
	}
}

func TestSpeakersWithMissingChannel(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, bindingsSpeakerID, deletedChannel)

	m := bindingsService(st, &bot.Client{Caches: cachesWithChannels(t, testGuildID, keptChannel)})
	m.statuses[testGuildID] = &guild.Status{
		GuildID:  testGuildID,
		Speakers: map[snowflake.ID]*guild.Speaker{bindingsSpeakerID: {ID: bindingsSpeakerID, Enabled: true}},
	}

	got := m.SpeakersWithMissingChannel(testGuildID)
	want := ChannelAccessWarning{BotID: bindingsSpeakerID, ChannelID: deletedChannel}
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %+v, want [%+v]", got, want)
	}
}

// When the owner bot's channel is deleted mid-raid, Discord also disconnects
// the bot, and the reconnect that follows can run before the binding is removed.
// It must give up on the gone channel rather than apply audio to a connection
// that was never opened.
func TestReconnectBotChannel_GivesUpWhenOwnerChannelIsGone(t *testing.T) {
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, deletedChannel)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("channel_bindings_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m := bindingsService(st, &bot.Client{VoiceManager: &recordingVoiceManager{}, Caches: cachesWithChannels(t, testGuildID, keptChannel)})
	m.metrics = metrics
	m.statuses[testGuildID] = &guild.Status{GuildID: testGuildID, Session: &guild.Session{GuildID: testGuildID}}
	m.storeApplier(testGuildID, testBotID, m.buildApplier(testGuildID, testBotID, nil, nil, nil))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reconnect to a deleted owner channel panicked: %v", r)
		}
	}()
	m.ReconnectBotChannel(context.Background(), testGuildID, testBotID)
}
