package bot

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// GuildCommandSyncer registers the owner bot's slash commands for one guild.
// Implemented by Bot.syncGuildCommands; may be nil when command registration is
// out of scope for the caller (the E2E harness drives handlers directly and
// never invites the bot to a new guild).
type GuildCommandSyncer func(ctx context.Context, guildID snowflake.ID)

// EventListeners returns all event listeners to register with the owner bot client.
// Called by the production bot and by the E2E harness so both use identical handler logic.
func EventListeners(managerSvc ManagerService, metrics *telemetry.BotMetrics, syncGuild GuildCommandSyncer) []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(onReady(managerSvc)),
		bot.NewListenerFunc(onGuildAvailable(managerSvc, metrics)),
		bot.NewListenerFunc(onGuildJoin(managerSvc, syncGuild)),
		bot.NewListenerFunc(onGuildMemberAdd(managerSvc)),
		bot.NewListenerFunc(onGuildMemberLeave(managerSvc)),
		bot.NewListenerFunc(onGuildMemberUpdate(managerSvc)),
		bot.NewListenerFunc(onVoiceJoin(managerSvc, metrics)),
		bot.NewListenerFunc(onVoiceLeave(managerSvc, metrics)),
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

// onGuildAvailable is called for each guild that becomes available after the
// initial Ready handshake. It initialises VoiceCallers from the current voice
// states so the counter is accurate after a bot restart (users already in
// voice channels emit no new join events).
func onGuildAvailable(m ManagerService, metrics *telemetry.BotMetrics) func(*events.GuildAvailable) {
	return func(e *events.GuildAvailable) {
		// Seed VoiceCallers from voice states present in the GUILD_CREATE payload.
		counts := make(map[snowflake.ID]int64) // channelID → caller count
		for _, vs := range e.Guild.VoiceStates {
			if vs.ChannelID == nil {
				continue
			}
			member, ok := e.Client().Caches.Member(e.GuildID, vs.UserID)
			if !ok || member.User.Bot {
				continue
			}
			if m.HasCallerRole(e.GuildID, member.RoleIDs) {
				counts[*vs.ChannelID]++
			}
		}
		for channelID, count := range counts {
			metrics.VoiceCallerAdd(context.Background(), count, e.GuildID.String(), channelID.String())
		}
	}
}

// onGuildJoin is called when the owner bot is added to a new guild.
// It registers the guild's slash commands, seeds speakers, and ensures the guild
// has a persistent relay code.
//
// GuildJoin fires only for genuinely new guilds. GuildAvailable, by contrast,
// fires for every guild on every gateway reconnect, so syncing there would
// re-register the whole command set against the REST API on each reconnect.
func onGuildJoin(m ManagerService, syncGuild GuildCommandSyncer) func(*events.GuildJoin) {
	return func(e *events.GuildJoin) {
		if syncGuild != nil {
			// Commands first: until they are registered the guild sees a bot that
			// appears to do nothing at all.
			go syncGuild(context.Background(), e.GuildID)
		}
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
// It overwrites the cache entry and pushes the new allow decision into the active
// session's AllowFilter so the next audio frame sees the updated role immediately.
func onGuildMemberUpdate(m ManagerService) func(*events.GuildMemberUpdate) {
	return func(e *events.GuildMemberUpdate) {
		if m.IsBot(e.Member.User) {
			return
		}
		e.Client().Caches.MemberCache().Put(e.GuildID, e.Member.User.ID, e.Member)
		m.NotifyMemberUpdate(e.GuildID, e.Member)
	}
}

// onVoiceJoin is called whenever a user joins a voice channel.
// It refreshes the member cache with the full member object from the event
// (VOICE_STATE_UPDATE payloads can carry partial members without RoleIDs, so
// we overwrite whatever disgo stored with the authoritative data from this event).
func onVoiceJoin(m ManagerService, metrics *telemetry.BotMetrics) func(*events.GuildVoiceJoin) {
	return func(e *events.GuildVoiceJoin) {
		if m.IsBot(e.Member.User) {
			return
		}
		guildID := e.VoiceState.GuildID

		// Always overwrite the cache entry with the full member (including RoleIDs)
		// so the AllowFilter fallback sees accurate role data for both humans and bots.
		e.Client().Caches.MemberCache().Put(guildID, e.Member.User.ID, e.Member)
		m.NotifyMemberUpdate(guildID, e.Member)

		allowed := m.HasCallerRole(guildID, e.Member.RoleIDs)
		slog.Info("user joined voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("channelID", e.VoiceState.ChannelID.String()),
			slog.Bool("allowedToSpeak", allowed),
		)

		if allowed {
			metrics.VoiceCallerAdd(context.Background(), 1, guildID.String(), e.VoiceState.ChannelID.String())
		}

		// Trigger an auto-route recompute on the channel the user joined.
		// The router owns both source-mode routing AND mixer pause state
		// (cascade ∧ listener check folded together — Plan §3.6 final).
		if e.VoiceState.ChannelID != nil {
			m.AutoRoute(guildID, *e.VoiceState.ChannelID)
		}
	}
}

// onVoiceLeave is called whenever a user leaves a voice channel.
func onVoiceLeave(m ManagerService, metrics *telemetry.BotMetrics) func(*events.GuildVoiceLeave) {
	return func(e *events.GuildVoiceLeave) {
		guildID := e.VoiceState.GuildID

		if e.Member.User.Bot {
			// Reconnect the bot to its bound channel if a session is active.
			if m.HasActiveSession(guildID) {
				go m.ReconnectBotChannel(context.Background(), guildID, e.Member.User.ID)
			}
			return
		}

		slog.Info("user left voice channel",
			slog.String("userID", e.Member.User.ID.String()),
			slog.String("guildID", guildID.String()),
		)

		if m.HasCallerRole(guildID, e.Member.RoleIDs) {
			metrics.VoiceCallerAdd(context.Background(), -1, guildID.String(), e.OldVoiceState.ChannelID.String())
		}

		// Trigger an auto-route recompute on the channel the user vacated.
		// e.VoiceState.ChannelID is nil after a leave (no channel); the
		// pre-event channel lives on e.OldVoiceState. The router pauses the
		// destination's mixer if this was the last listener.
		if e.OldVoiceState.ChannelID != nil {
			m.AutoRoute(guildID, *e.OldVoiceState.ChannelID)
		}
	}
}

// onVoiceMove is called whenever a user moves between voice channels.
func onVoiceMove(m ManagerService) func(*events.GuildVoiceMove) {
	return func(e *events.GuildVoiceMove) {
		guildID := e.VoiceState.GuildID

		if e.Member.User.Bot {
			// Delegate bot-move business logic to the manager: it checks whether the
			// bot was displaced from its bound channel and reconnects if needed.
			m.OnBotVoiceMove(context.Background(), guildID, e.Member.User.ID, e.VoiceState.ChannelID)
			return
		}

		// Auto-route both channels: the source channel loses a caller, the
		// destination gains one. The router debounces, so two same-instant
		// triggers on different channels still collapse to one Recompute per
		// channel after the window. Pause state for both channels is also
		// updated as part of the same Recompute.
		if e.OldVoiceState.ChannelID != nil {
			m.AutoRoute(guildID, *e.OldVoiceState.ChannelID)
		}
		if e.VoiceState.ChannelID != nil {
			m.AutoRoute(guildID, *e.VoiceState.ChannelID)
		}
	}
}
