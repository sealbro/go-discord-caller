package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func voiceChannel(t *testing.T, guildID, channelID snowflake.ID) discord.GuildVoiceChannel {
	t.Helper()
	var ch discord.GuildVoiceChannel
	raw := fmt.Sprintf(`{"id":"%s","guild_id":"%s","type":2,"name":"voice"}`, channelID, guildID)
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		t.Fatalf("unmarshal voice channel: %v", err)
	}
	return ch
}

// cachesWithChannels returns owner-bot caches in which guildID has exactly the
// given voice channels.
func cachesWithChannels(t *testing.T, guildID snowflake.ID, channelIDs ...snowflake.ID) cache.Caches {
	t.Helper()
	caches := cache.New(cache.WithCaches(cache.FlagsAll))
	for _, channelID := range channelIDs {
		caches.AddChannel(voiceChannel(t, guildID, channelID))
	}
	return caches
}

// speakerJoinAttempted runs setupSpeakers for one speaker bound to boundChannel,
// with existingChannel the only voice channel the guild has, and reports whether
// the speaker tried to join.
func speakerJoinAttempted(t *testing.T, boundChannel, existingChannel snowflake.ID) bool {
	t.Helper()
	const speakerID = snowflake.ID(500)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, speakerID, boundChannel)

	var joined atomic.Bool
	speakerVM := &fakeVoiceManager{
		conn:   &fakeVoiceConn{channelID: boundChannel},
		onOpen: func() { joined.Store(true) },
	}
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("missing_channel_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m := &Service{
		statuses: map[snowflake.ID]*guild.Status{
			testGuildID: {
				GuildID:     testGuildID,
				OwnerUserID: testBotID,
				Speakers:    map[snowflake.ID]*guild.Speaker{speakerID: {ID: speakerID, Enabled: true}},
			},
		},
		store:       st,
		poolSvc:     onePool{id: speakerID, client: &bot.Client{VoiceManager: speakerVM, Caches: cache.New(), Rest: stubRest{}}},
		ownerClient: &bot.Client{Caches: cachesWithChannels(t, testGuildID, existingChannel), Rest: stubRest{}},
		ownerBotID:  testBotID,
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}

	ctx := context.Background()
	setup, _ := m.setupSpeakers(ctx, testGuildID, guild.RaidModeOneCaller, nil, metrics.ForGuild(ctx, testGuildID))
	if setup != nil {
		setup.SpeakerCleanup()
	}
	return joined.Load()
}

// A speaker bound to a channel that no longer exists can never join it, and
// trying costs the whole join budget (15 s + 5 s cleanup) before the owner bot
// may connect. The guild's channels are already cached, so it must be skipped
// without an attempt.
func TestSetupSpeakers_SkipsSpeakerWhoseChannelNoLongerExists(t *testing.T) {
	if speakerJoinAttempted(t, snowflake.ID(901), snowflake.ID(900)) {
		t.Error("speaker bound to a deleted channel tried to join it")
	}
}

func TestSetupSpeakers_JoinsSpeakerWhoseChannelExists(t *testing.T) {
	if !speakerJoinAttempted(t, snowflake.ID(900), snowflake.ID(900)) {
		t.Error("speaker bound to an existing channel did not try to join it")
	}
}
