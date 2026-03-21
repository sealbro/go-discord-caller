package bot

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/caller"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

// eventListeners returns all event listeners to register with the client.
func eventListeners(c *caller.Caller, m *manager.Service) []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(onReady(m)),
		bot.NewListenerFunc(onVoiceJoin(c)),
		bot.NewListenerFunc(onVoiceLeave(c)),
	}
}

// onReady is called when the bot has connected and is ready.
// It seeds the speaker list with any pool bots already joined to each guild.
func onReady(m *manager.Service) func(*events.Ready) {
	return func(e *events.Ready) {
		slog.Info("bot is ready", slog.String("username", e.User.Username))

		guildIDs := make([]snowflake.ID, 0, len(e.Guilds))
		for _, g := range e.Guilds {
			guildIDs = append(guildIDs, g.ID)
		}

		go m.SeedExistingSpeakers(context.Background(), guildIDs)
	}
}

// ...existing code...

// onVoiceJoin is called whenever a user joins a voice channel.
func onVoiceJoin(c *caller.Caller) func(*events.GuildVoiceJoin) {
	return func(e *events.GuildVoiceJoin) {
		// Ignore the bot's own voice state changes.
		if e.Member.User.ID == e.Client().ID() {
			return
		}

		channelID := e.VoiceState.ChannelID
		slog.Info("user joined voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("channelID", channelID.String()),
		)
		// TODO: add your call logic here (e.g. join when a user enters a channel)
	}
}

// onVoiceLeave is called whenever a user leaves a voice channel.
func onVoiceLeave(c *caller.Caller) func(*events.GuildVoiceLeave) {
	return func(e *events.GuildVoiceLeave) {
		// Ignore the bot's own voice state changes.
		if e.Member.User.ID == e.Client().ID() {
			return
		}

		slog.Info("user left voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("guildID", e.VoiceState.GuildID.String()),
		)
	}
}
