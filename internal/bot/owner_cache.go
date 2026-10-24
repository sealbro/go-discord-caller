package bot

import (
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/bot/handlers"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
)

// ownerCacheFlags are the caches the owner bot reads. Everything else disgo
// can cache (messages, emojis, stickers, threads, …) is held per guild for the
// life of the process, so it is left off to keep idle guilds cheap.
const ownerCacheFlags = cache.FlagGuilds |
	cache.FlagChannels |
	cache.FlagRoles |
	cache.FlagMembers |
	cache.FlagVoiceStates

// ownerCacheOpts keeps a member only while they are in voice, plus every bot.
// Role checks only ever concern users in a voice channel, and a guild's full
// member list would otherwise accumulate from member-update events.
//
// The policy needs the caches it is installed in, which do not exist until the
// client is built, so the caller sets *caches right after disgo.New.
func ownerCacheOpts(caches *cache.Caches) []cache.ConfigOpt {
	return []cache.ConfigOpt{
		cache.WithCaches(ownerCacheFlags),
		cache.WithMemberCachePolicy(func(m discord.Member) bool {
			if m.User.Bot || *caches == nil {
				return true
			}
			_, inVoice := (*caches).VoiceState(m.GuildID, m.User.ID)
			return inVoice
		}),
	}
}

// ownerCacheListeners keep the member cache consistent with ownerCacheOpts.
func ownerCacheListeners() []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(func(e *events.GuildVoiceLeave) {
			evictVoiceLeaver(e.Client().Caches, e.Member)
		}),
	}
}

// ownerGatewayHandlers runs disgo's GUILD_CREATE handler, then re-adds the
// guild's members. disgo caches them before the guild's voice states, so the
// policy rejects everyone in voice on that first pass, and the guild events it
// dispatches no longer carry the member list.
func ownerGatewayHandlers() bot.ConfigOpt {
	gatewayHandlers := handlers.GetGatewayHandlers()
	guildCreate := gatewayHandlers[gateway.EventTypeGuildCreate]
	gatewayHandlers[gateway.EventTypeGuildCreate] = bot.NewGatewayEventHandler(gateway.EventTypeGuildCreate,
		func(client *bot.Client, sequenceNumber int, shardID int, event gateway.EventGuildCreate) {
			guildCreate.HandleGatewayEvent(client, sequenceNumber, shardID, event)
			cacheVoiceMembers(client.Caches, event.ID, event.Members)
		})
	return bot.WithEventManagerConfigOpts(bot.WithGatewayHandlers(gatewayHandlers))
}

func cacheVoiceMembers(caches cache.Caches, guildID snowflake.ID, members []discord.Member) {
	for _, m := range members {
		m.GuildID = guildID
		caches.AddMember(m)
	}
}

func evictVoiceLeaver(caches cache.Caches, m discord.Member) {
	if m.User.Bot {
		return
	}
	caches.RemoveMember(m.GuildID, m.User.ID)
}
