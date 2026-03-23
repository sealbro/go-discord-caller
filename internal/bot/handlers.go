package bot

import (
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

// eventListeners returns all event listeners to register with the client.
func eventListeners(managerSvc *manager.Service) []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(onReady(managerSvc)),
		bot.NewListenerFunc(onGuildMemberAdd(managerSvc)),
		bot.NewListenerFunc(onGuildMemberLeave(managerSvc)),
		bot.NewListenerFunc(onVoiceJoin()),
		bot.NewListenerFunc(onVoiceLeave()),
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

		go m.SeedExistingSpeakers(guildIDs)
	}
}

// onGuildMemberAdd is called whenever a new member joins a guild.
// If the member is an unregistered pool speaker bot it is automatically
// registered, mirroring the startup seeding logic in SeedExistingSpeakers.
func onGuildMemberAdd(m *manager.Service) func(*events.GuildMemberJoin) {
	return func(e *events.GuildMemberJoin) {
		go m.TrySeedMember(e.GuildID, e.Member.User.ID)
	}
}

// onGuildMemberLeave is called whenever a member leaves a guild.
// If the leaving member is a registered speaker bot it is removed from the guild status.
func onGuildMemberLeave(m *manager.Service) func(leave *events.GuildMemberLeave) {
	return func(e *events.GuildMemberLeave) {
		go m.RemoveSpeaker(e.GuildID, e.User.ID)
	}
}

// onVoiceJoin is called whenever a user joins a voice channel.
func onVoiceJoin() func(*events.GuildVoiceJoin) {
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
func onVoiceLeave() func(*events.GuildVoiceLeave) {
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
