package bot

import (
	"log/slog"
	"strconv"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// handleSpeakersPage opens (or navigates to) a speaker bind page.
func (h *CommandHandlers) handleSpeakersPage(guildID snowflake.ID, loc *i18n.Localizer, _ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.WarnContext(e.Ctx, "handleSpeakersPage: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	msg, components := h.buildSpeakersPageMessage(guildID, loc, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleRolesMenu opens the roles bind page.
func (h *CommandHandlers) handleRolesMenu(guildID snowflake.ID, loc *i18n.Localizer, _ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	msg, components := h.buildRolesPageMessage(guildID, loc)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleMainMenu returns the user to the main setup message.
func (h *CommandHandlers) handleMainMenu(guildID snowflake.ID, loc *i18n.Localizer, _ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	msg, components := h.buildMainSetupMessage(guildID, loc)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindRoleMenu handles capture role selection from the roles page and refreshes it.
//
// Rejected while a voice raid is active: the auto-router captures the caller
// role at session start and a mid-session change would leave the cached roleID
// stale (plan §3.8). The user must /stop the raid before rebinding.
func (h *CommandHandlers) handleBindRoleMenu(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	if h.manager.HasActiveSession(guildID) {
		return e.CreateMessage(ephemeral(loc.T("setup.blocked_active_raid")))
	}
	return h.applyRoleMenuBinding(guildID, loc, store.RoleTypeCaller, data, e)
}

// handleBindManagerRoleMenu handles manager role selection from the roles page and refreshes it.
func (h *CommandHandlers) handleBindManagerRoleMenu(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	return h.applyRoleMenuBinding(guildID, loc, store.RoleTypeManager, data, e)
}

// handleBindLocale handles the /setup language dropdown. An empty value clears
// the guild pin and reverts to per-user interaction locale. The main setup page
// is redrawn using the newly resolved locale.
func (h *CommandHandlers) handleBindLocale(guildID snowflake.ID, _ *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	stringData, ok := data.(discord.StringSelectMenuInteractionData)
	if !ok {
		// No localizer context safe to assume; fall back to English.
		return e.CreateMessage(ephemeral(h.bundle.For("", e.Locale()).T("err.unexpected_data_type")))
	}

	values := stringData.Values
	if len(values) == 0 || values[0] == localeAutoValue {
		h.manager.UnbindLocale(guildID)
	} else {
		h.manager.BindLocale(guildID, values[0])
	}

	// Re-resolve the localizer so the redraw uses the newly pinned language.
	newLoc := h.loc(guildID, e.Locale())
	msg, components := h.buildMainSetupMessage(guildID, newLoc)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// applyRoleMenuBinding handles the shared logic for role select menus: type-asserts the data,
// calls bind or unbind depending on whether a role was selected, then refreshes the roles page.
func (h *CommandHandlers) applyRoleMenuBinding(
	guildID snowflake.ID,
	loc *i18n.Localizer,
	roleType store.RoleType,
	data discord.SelectMenuInteractionData,
	e *handler.ComponentEvent,
) error {
	roleData, ok := data.(discord.RoleSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral(loc.T("err.unexpected_data_type")))
	}

	roles := roleData.Roles()
	if len(roles) == 0 {
		h.manager.UnbindRole(guildID, roleType)
	} else {
		h.manager.BindRole(guildID, roleType, roles[0].ID)
	}

	msg, components := h.buildRolesPageMessage(guildID, loc)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleToggleSpeaker enables or disables a speaker and refreshes the speaker page.
func (h *CommandHandlers) handleToggleSpeaker(guildID snowflake.ID, loc *i18n.Localizer, _ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral(loc.T("err.invalid_speaker_id")))
	}

	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.WarnContext(e.Ctx, "handleToggleSpeaker: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	status := h.manager.GetStatus(guildID)
	sp, ok := status.Speakers[speakerID]
	if !ok {
		return e.CreateMessage(ephemeral(loc.T("err.speaker_not_found")))
	}

	if err := h.manager.ToggleSpeaker(guildID, speakerID, !sp.Enabled); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	msg, components := h.buildSpeakersPageMessage(guildID, loc, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleAddSpeakerButton navigates to the "Add Speaker" sub-page.
// It resolves the next uninvited pool bot, builds a Discord OAuth2 invite URL
// pre-targeted at this guild, and shows a link button alongside a Main Menu return.
// The bot is registered automatically via the GuildMemberJoin event once it accepts the invite.
func (h *CommandHandlers) handleAddSpeakerButton(guildID snowflake.ID, loc *i18n.Localizer, _ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	botUserID, ok := h.manager.NextSpeakerID(guildID)
	if !ok {
		return e.CreateMessage(ephemeral(loc.T("err.no_more_tokens")))
	}

	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(loc.T("speaker.add_title")+"\n"+loc.T("speaker.add_steps")).
		WithComponents(
			discord.NewActionRow(
				discord.NewLinkButton(loc.T("btn.invite"), installURL(botUserID, guildID)),
			),
			discord.NewActionRow(
				discord.NewSecondaryButton(loc.T("btn.main_menu"), "/speakers/menu"),
			),
		))
}

// handleBindChannel updates the voice channel bound to a speaker and refreshes the speaker page.
func (h *CommandHandlers) handleBindChannel(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral(loc.T("err.invalid_speaker_id")))
	}

	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.WarnContext(e.Ctx, "handleBindChannel: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	channelData, ok := data.(discord.ChannelSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral(loc.T("err.unexpected_data_type")))
	}

	channels := channelData.Channels()
	if len(channels) == 0 {
		h.manager.UnbindChannel(guildID, speakerID)
	} else {
		h.manager.BindChannel(guildID, speakerID, channels[0].ID)
	}

	msg, components := h.buildSpeakersPageMessage(guildID, loc, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindOwnerChannel updates the owner bot's voice channel and refreshes the main setup message.
func (h *CommandHandlers) handleBindOwnerChannel(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	channelData, ok := data.(discord.ChannelSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral(loc.T("err.unexpected_data_type")))
	}

	ownerBotID := h.manager.OwnerBotID()
	channels := channelData.Channels()
	if len(channels) == 0 {
		h.manager.UnbindChannel(guildID, ownerBotID)
	} else {
		h.manager.BindChannel(guildID, ownerBotID, channels[0].ID)
	}

	msg, components := h.buildMainSetupMessage(guildID, loc)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}
