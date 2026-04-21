package bot

import (
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// eventListeners returns all event listeners to register with the client.
func eventListeners(managerSvc ManagerService) []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(onReady(managerSvc)),
		bot.NewListenerFunc(onGuildJoin(managerSvc)),
		bot.NewListenerFunc(onGuildMemberAdd(managerSvc)),
		bot.NewListenerFunc(onGuildMemberLeave(managerSvc)),
		bot.NewListenerFunc(onGuildMemberUpdate),
		bot.NewListenerFunc(onVoiceJoin(managerSvc)),
		bot.NewListenerFunc(onVoiceLeave(managerSvc)),
		bot.NewListenerFunc(onVoiceMove(managerSvc)),
	}
}

// onReady is called when the bot has connected and is ready.
// It seeds the speaker list with any pool bots already joined to each guild.
func onReady(m ManagerService) func(*events.Ready) {
	return func(e *events.Ready) {
		slog.Info("bot is ready", slog.String("username", e.User.Username))

		guildIDs := make([]snowflake.ID, 0, len(e.Guilds))
		for _, g := range e.Guilds {
			guildIDs = append(guildIDs, g.ID)
		}

		go m.SeedExistingSpeakers(guildIDs)
	}
}

// onGuildJoin is called when the owner bot is added to a new guild.
// It seeds speakers and ensures the guild has a persistent relay code.
func onGuildJoin(m ManagerService) func(*events.GuildJoin) {
	return func(e *events.GuildJoin) {
		go m.SeedExistingSpeakers([]snowflake.ID{e.GuildID})
	}
}

// onGuildMemberAdd is called whenever a new member joins a guild.
// If the member is an unregistered pool speaker bot it is automatically
// registered, mirroring the startup seeding logic in SeedExistingSpeakers.
func onGuildMemberAdd(m ManagerService) func(*events.GuildMemberJoin) {
	return func(e *events.GuildMemberJoin) {
		if !e.Member.User.Bot {
			return
		}

		go m.TrySeedMember(e.GuildID, e.Member.User.ID)
	}
}

// onGuildMemberLeave is called whenever a member leaves a guild.
// If the leaving member is a registered speaker bot it is removed from the guild status.
func onGuildMemberLeave(m ManagerService) func(leave *events.GuildMemberLeave) {
	return func(e *events.GuildMemberLeave) {
		if !e.User.Bot {
			return
		}

		go m.RemoveSpeaker(e.GuildID, e.User.ID)
	}
}

// onGuildMemberUpdate is called whenever a guild member is updated (e.g. role change).
// It overwrites the cache entry so that the allowUser role filter picks up the new
// RoleIDs on the very next audio frame — without requiring a session restart.
func onGuildMemberUpdate(e *events.GuildMemberUpdate) {
	if e.Member.User.Bot {
		return
	}

	e.Client().Caches.MemberCache().Put(e.GuildID, e.Member.User.ID, e.Member)
}

// onVoiceJoin is called whenever a user joins a voice channel.
// It refreshes the member cache with the full member object from the event
// (VOICE_STATE_UPDATE payloads can carry partial members without RoleIDs, so
// we overwrite whatever disgo stored with the authoritative data from this event).
func onVoiceJoin(m ManagerService) func(*events.GuildVoiceJoin) {
	return func(e *events.GuildVoiceJoin) {
		if e.Member.User.Bot {
			return
		}

		guildID := e.VoiceState.GuildID

		// Overwrite the cache entry with the full member (including RoleIDs).
		e.Client().Caches.MemberCache().Put(guildID, e.Member.User.ID, e.Member)

		allowed := m.HasCallerRole(guildID, e.Member.RoleIDs)
		slog.Info("user joined voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("channelID", e.VoiceState.ChannelID.String()),
			slog.Bool("allowedToSpeak", allowed),
		)

		// A non-bot user appeared — resume the mixer for this channel if paused.
		m.UpdateMixerPause(guildID)
	}
}

// onVoiceLeave is called whenever a user leaves a voice channel.
func onVoiceLeave(m ManagerService) func(*events.GuildVoiceLeave) {
	return func(e *events.GuildVoiceLeave) {
		if e.Member.User.Bot {
			return
		}

		slog.Info("user left voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("guildID", e.VoiceState.GuildID.String()),
		)

		// A non-bot user left — pause the mixer if this was the last listener.
		m.UpdateMixerPause(e.VoiceState.GuildID)
	}
}

// onVoiceMove is called whenever a user moves between voice channels.
func onVoiceMove(m ManagerService) func(*events.GuildVoiceMove) {
	return func(e *events.GuildVoiceMove) {
		if e.Member.User.Bot {
			return
		}

		// Both the old and new channel may need mixer pause state updated.
		m.UpdateMixerPause(e.VoiceState.GuildID)
	}
}
