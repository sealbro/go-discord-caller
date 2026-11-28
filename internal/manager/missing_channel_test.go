package manager

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
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
	var joined atomic.Bool
	speakerVM := &fakeVoiceManager{
		conn:   &fakeVoiceConn{channelID: boundChannel},
		onOpen: func() { joined.Store(true) },
	}
	_ = runOneCallerSetup(oneSpeakerService(t, snowflake.ID(500), boundChannel, speakerVM, stubRest{}, existingChannel))
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
