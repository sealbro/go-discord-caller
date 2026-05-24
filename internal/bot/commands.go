package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// BuildCommands returns the slash command list with localized descriptions
// (and option choice names) attached from the i18n bundle. Command names stay
// in English; only descriptions and option choices are localized.
func BuildCommands(bundle *i18n.Bundle) []discord.ApplicationCommandCreate {
	en := bundle.For("", "")

	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:                     "setup",
			Description:              en.T("cmd.setup.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.setup.description"),
		},
		discord.SlashCommandCreate{
			Name:                     "start",
			Description:              en.T("cmd.start.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.description"),
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:                     "code",
					Description:              en.T("cmd.start.opt.code.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.opt.code.description"),
					Required:                 false,
				},
				discord.ApplicationCommandOptionString{
					Name:                     "mode",
					Description:              en.T("cmd.start.opt.mode.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.opt.mode.description"),
					Required:                 false,
					Choices: []discord.ApplicationCommandOptionChoiceString{
						{Name: en.T("cmd.start.opt.mode.choice.one"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.one"), Value: callerModeOne},
						{Name: en.T("cmd.start.opt.mode.choice.many"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.many"), Value: callerModeMany},
						{Name: en.T("cmd.start.opt.mode.choice.one_many"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.one_many"), Value: callerModeOneMany},
					},
				},
			},
		},
		discord.SlashCommandCreate{
			Name:                     "stop",
			Description:              en.T("cmd.stop.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.stop.description"),
		},
		discord.SlashCommandCreate{
			Name:                     "status",
			Description:              en.T("cmd.status.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.status.description"),
		},
		discord.SlashCommandCreate{
			Name:                     "bind-role",
			Description:              en.T("cmd.bind_role.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.bind_role.description"),
			DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionRole{
					Name:                     "role",
					Description:              en.T("cmd.bind_role.opt.role.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.bind_role.opt.role.description"),
					Required:                 true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:                     "bind-manager-role",
			Description:              en.T("cmd.bind_manager_role.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.bind_manager_role.description"),
			DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionRole{
					Name:                     "role",
					Description:              en.T("cmd.bind_manager_role.opt.role.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.bind_manager_role.opt.role.description"),
					Required:                 true,
				},
			},
		},
	}
}

// permPtr wraps a Permissions value into the omit.Omit[*discord.Permissions] type
// required by SlashCommandCreate.DefaultMemberPermissions.
func permPtr(p discord.Permissions) omit.Omit[*discord.Permissions] {
	return omit.New(&p)
}

// CommandHandlers wires all slash command and component routes to the manager service.
type CommandHandlers struct {
	manager ManagerService
	metrics *telemetry.BotMetrics
	bundle  *i18n.Bundle
}

// NewCommandHandlers creates a new CommandHandlers.
func NewCommandHandlers(m ManagerService, metrics *telemetry.BotMetrics, bundle *i18n.Bundle) *CommandHandlers {
	return &CommandHandlers{manager: m, metrics: metrics, bundle: bundle}
}

// loc returns a localizer for guildID, picking the guild-pinned locale (if any)
// over the user's Discord client locale.
func (h *CommandHandlers) loc(guildID snowflake.ID, interactionLocale discord.Locale) *i18n.Localizer {
	return h.bundle.For(h.manager.GetLocale(guildID), interactionLocale)
}

// Register attaches all routes to the given router.
func (h *CommandHandlers) Register(r handler.Router) {
	r.SlashCommand("/setup", h.withAdmin(h.handleSetup))
	r.SlashCommand("/start", h.withManager(h.handleStartVoiceRaid))
	r.SlashCommand("/stop", h.withManager(h.handleStopVoiceRaid))
	r.SlashCommand("/status", h.withGuild(h.handleStatus))
	r.SlashCommand("/bind-role", h.withGuild(h.handleBindRole))
	r.SlashCommand("/bind-manager-role", h.withGuild(h.handleBindManagerRole))

	// Main setup menu components
	r.SelectMenuComponent("/setup/bind-role", h.withGuildSelectMenu(h.handleBindRoleMenu))
	r.SelectMenuComponent("/setup/bind-manager-role", h.withGuildSelectMenu(h.handleBindManagerRoleMenu))
	r.SelectMenuComponent("/setup/locale", h.withGuildSelectMenu(h.handleBindLocale))
	r.SelectMenuComponent("/owner/bind-channel", h.withGuildSelectMenu(h.handleBindOwnerChannel))
	r.ButtonComponent("/roles/menu", h.withGuildButton(h.handleRolesMenu))
	r.ButtonComponent("/speakers/page/{page}", h.withGuildButton(h.handleSpeakersPage))
	r.ButtonComponent("/speakers/menu", h.withGuildButton(h.handleMainMenu))

	// Speaker page components (page number is embedded in the custom ID)
	r.ButtonComponent("/speakers/toggle/{speakerID}/{page}", h.withGuildButton(h.handleToggleSpeaker))
	r.ButtonComponent("/speakers/add", h.withGuildButton(h.handleAddSpeakerButton))
	r.SelectMenuComponent("/speakers/bind-channel/{speakerID}/{page}", h.withGuildSelectMenu(h.handleBindChannel))
}

// ── Constants ─────────────────────────────────────────────────────────────────

// callerModeChoice is the value of the "mode" slash command option.
// It maps to different RaidModes depending on whether a relay code is supplied:
//   - no code (host): callerModeOne → RaidModeOneCaller, callerModeMany → RaidModeGuildCaller, callerModeOneMany → RaidModeOneManyGuildCaller
//   - with code (guest): callerModeOne → RaidModeAllyListener, callerModeMany → RaidModeAllyCaller, callerModeOneMany → RaidModeOneManyAllyCaller
const (
	callerModeOne     string = "one"
	callerModeMany    string = "many"
	callerModeOneMany string = "one_many"
)

// speakersPerPage is the maximum number of speakers shown per page in the
// speaker bind menu.  Discord allows 5 action rows per message:
//   - row 1 = toggle buttons
//   - rows 2-4 = channel-select menus (one per speaker)
//   - row 5 = navigation buttons
const speakersPerPage = 3

// ── Middleware ────────────────────────────────────────────────────────────────

// guildCommandHandler is a slash command handler that receives a validated guild ID and localizer.
type guildCommandHandler func(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error

// withGuild wraps a handler to validate the guild context and create an OTel span.
func (h *CommandHandlers) withGuild(fn guildCommandHandler) func(discord.SlashCommandInteractionData, *handler.CommandEvent) error {
	return func(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
		guildID, errMsg := requireGuild(h.bundle.For("", e.Locale()), e.GuildID())
		if errMsg != nil {
			return e.CreateMessage(*errMsg)
		}
		loc := h.loc(guildID, e.Locale())

		ctx, span := telemetry.Tracer.Start(e.Ctx, "discord.command",
			trace.WithAttributes(
				attribute.String("command", data.CommandName()),
				attribute.String("guild.id", guildID.String()),
				attribute.String("user.id", e.User().ID.String()),
			),
		)
		start := time.Now()
		e.Ctx = ctx
		err := fn(guildID, loc, data, e)
		duration := time.Since(start).Seconds()

		h.metrics.RecordCommand(ctx, data.CommandName(), guildID.String(), duration)

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()

		return err
	}
}

// withManager wraps a handler to require Manage Server permission or the manager role.
func (h *CommandHandlers) withManager(fn guildCommandHandler) func(discord.SlashCommandInteractionData, *handler.CommandEvent) error {
	return h.withGuild(func(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
		if !h.isManagerAuthorized(guildID, e.Member()) {
			return e.CreateMessage(ephemeral(loc.T("err.need_manager")))
		}
		return fn(guildID, loc, data, e)
	})
}

// withAdmin wraps a handler to require Administrator permission or the manager role.
func (h *CommandHandlers) withAdmin(fn guildCommandHandler) func(discord.SlashCommandInteractionData, *handler.CommandEvent) error {
	return h.withGuild(func(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
		if !h.isAdminAuthorized(guildID, e.Member()) {
			return e.CreateMessage(ephemeral(loc.T("err.need_admin")))
		}
		return fn(guildID, loc, data, e)
	})
}

// guildButtonHandler is a button component handler that receives a validated guild ID and localizer.
type guildButtonHandler func(guildID snowflake.ID, loc *i18n.Localizer, data discord.ButtonInteractionData, e *handler.ComponentEvent) error

// withGuildButton wraps a button component handler to validate the guild context.
func (h *CommandHandlers) withGuildButton(fn guildButtonHandler) func(discord.ButtonInteractionData, *handler.ComponentEvent) error {
	return func(data discord.ButtonInteractionData, e *handler.ComponentEvent) error {
		guildID, errMsg := requireGuild(h.bundle.For("", e.Locale()), e.GuildID())
		if errMsg != nil {
			return e.CreateMessage(*errMsg)
		}
		return fn(guildID, h.loc(guildID, e.Locale()), data, e)
	}
}

// guildSelectMenuHandler is a select menu component handler that receives a validated guild ID and localizer.
type guildSelectMenuHandler func(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error

// withGuildSelectMenu wraps a select menu component handler to validate the guild context.
func (h *CommandHandlers) withGuildSelectMenu(fn guildSelectMenuHandler) func(discord.SelectMenuInteractionData, *handler.ComponentEvent) error {
	return func(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
		guildID, errMsg := requireGuild(h.bundle.For("", e.Locale()), e.GuildID())
		if errMsg != nil {
			return e.CreateMessage(*errMsg)
		}
		return fn(guildID, h.loc(guildID, e.Locale()), data, e)
	}
}

// ── Setup message builders ────────────────────────────────────────────────────

// buildMainSetupMessage builds the main setup message.
//
// Layout:
//   - Row 1: language select menu (per-guild locale pin)
//   - Row 2: owner voice-channel select menu
//   - Row 3: "Bind Roles", "Bind Speakers" buttons; "Add Speaker" appended when an uninvited pool bot is available
func (h *CommandHandlers) buildMainSetupMessage(guildID snowflake.ID, loc *i18n.Localizer) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	ownerBotID := h.manager.OwnerBotID()
	var components []discord.LayoutComponent

	// Row 1 — language select
	components = append(components, discord.NewActionRow(h.buildLocaleSelect(guildID, loc)))

	// Row 2 — owner bot channel selector
	ownerMenu := discord.NewChannelSelectMenu("/owner/bind-channel", loc.T("setup.bind_caller_placeholder")).
		WithChannelTypes(discord.ChannelTypeGuildVoice)
	if chID, ok := h.manager.GetBoundChannel(guildID, ownerBotID); ok {
		ownerMenu = ownerMenu.AddDefaultValue(chID)
	}
	components = append(components, discord.NewActionRow(ownerMenu))

	// Row 3 — action buttons
	buttons := []discord.InteractiveComponent{
		discord.NewPrimaryButton(loc.T("btn.bind_roles"), "/roles/menu"),
		discord.NewPrimaryButton(loc.T("btn.bind_speakers"), "/speakers/page/0"),
	}
	if h.manager.HasAvailableToken(guildID) {
		buttons = append(buttons, discord.NewSuccessButton(loc.T("btn.add_speaker"), "/speakers/add"))
	}
	components = append(components, discord.NewActionRow(buttons...))

	return loc.T("setup.main_title") + "\n" + status.Render(loc), components
}

// buildLocaleSelect builds the per-guild language select menu. The currently
// pinned locale (if any) is marked as default; "Auto" represents no pin.
func (h *CommandHandlers) buildLocaleSelect(guildID snowflake.ID, loc *i18n.Localizer) discord.StringSelectMenuComponent {
	pinned := h.manager.GetLocale(guildID)

	autoOpt := discord.NewStringSelectMenuOption(loc.T("setup.locale_auto"), "")
	if pinned == "" {
		autoOpt = autoOpt.WithDefault(true)
	}
	options := []discord.StringSelectMenuOption{autoOpt}
	for _, bl := range i18n.SupportedBundleLocales() {
		opt := discord.NewStringSelectMenuOption(i18n.DisplayName(bl), bl)
		if bl == pinned {
			opt = opt.WithDefault(true)
		}
		options = append(options, opt)
	}
	return discord.NewStringSelectMenu("/setup/locale", loc.T("setup.locale_placeholder"), options...)
}

// buildRolesPageMessage builds the "Bind Roles" sub-page.
//
// Layout:
//   - Row 1: capture role select menu (pre-filled with current binding)
//   - Row 2: manager role select menu (pre-filled with current binding)
//   - Row 3: Main Menu button
func (h *CommandHandlers) buildRolesPageMessage(guildID snowflake.ID, loc *i18n.Localizer) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	var components []discord.LayoutComponent

	// Row 1 — capture role selector
	roleMenu := discord.NewRoleSelectMenu("/setup/bind-role", loc.T("setup.select_caller_role"))
	if status.CallerRoleID != nil {
		roleMenu = roleMenu.AddDefaultValue(*status.CallerRoleID)
	}
	components = append(components, discord.NewActionRow(roleMenu))

	// Row 2 — manager role selector
	managerRoleMenu := discord.NewRoleSelectMenu("/setup/bind-manager-role", loc.T("setup.select_manager_role"))
	if status.ManagerRoleID != nil {
		managerRoleMenu = managerRoleMenu.AddDefaultValue(*status.ManagerRoleID)
	}
	components = append(components, discord.NewActionRow(managerRoleMenu))

	// Row 3 — navigation
	components = append(components, discord.NewActionRow(
		discord.NewSecondaryButton(loc.T("btn.main_menu"), "/speakers/menu"),
	))

	return loc.T("setup.roles_title") + "\n" + status.Render(loc), components
}

// buildSpeakersPageMessage builds a paginated "Bind Speakers" sub-page.
//
// Layout (≤5 action rows, Discord limit):
//   - Row 1: enable/disable toggle button per speaker on this page
//   - Rows 2–4: voice-channel select menu per speaker (up to speakersPerPage = 3)
//   - Row 5: "🏠 Main Menu" + page-range jump buttons ("1-3", "4-6", …)
//
// Navigation uses a sliding window of up to maxPageBtns (4) pages centred on
// the current page. The current page's button is primary+disabled; others are secondary.
func (h *CommandHandlers) buildSpeakersPageMessage(guildID snowflake.ID, loc *i18n.Localizer, page int) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	speakers := status.GetSortedSpeakers()

	totalPages := max((len(speakers)+speakersPerPage-1)/speakersPerPage, 1)
	page = min(max(page, 0), totalPages-1)

	start := page * speakersPerPage
	end := min(start+speakersPerPage, len(speakers))
	pageSpeakers := speakers[start:end]

	var components []discord.LayoutComponent

	// Row 1 — toggle buttons
	var buttons []discord.InteractiveComponent
	enableLabel := loc.T("btn.enable")
	disableLabel := loc.T("btn.disable")
	for _, sp := range pageSpeakers {
		label := enableLabel
		if sp.Enabled {
			label = disableLabel
		}
		buttons = append(buttons, discord.NewSecondaryButton(
			fmt.Sprintf("%s %s (%s)", statusEmoji(sp.Enabled), sp.Username, label),
			fmt.Sprintf("/speakers/toggle/%s/%d", sp.ID, page),
		))
	}
	if len(buttons) > 0 {
		components = append(components, discord.NewActionRow(buttons...))
	}

	// Rows 2-4 — one channel select per speaker on this page
	for _, sp := range pageSpeakers {
		spMenu := discord.NewChannelSelectMenu(
			fmt.Sprintf("/speakers/bind-channel/%s/%d", sp.ID, page),
			loc.T("setup.bind_speaker_placeholder", "Username", sp.Username),
		).WithChannelTypes(discord.ChannelTypeGuildVoice)
		if chID, ok := h.manager.GetBoundChannel(guildID, sp.ID); ok {
			spMenu = spMenu.AddDefaultValue(chID)
		}
		components = append(components, discord.NewActionRow(spMenu))
	}

	// Row 5 — navigation: [Main Menu] + up to 4 page-range jump buttons.
	const maxPageBtns = 4
	windowStart, windowEnd := 0, totalPages
	if totalPages > maxPageBtns {
		half := maxPageBtns / 2
		windowStart = page - half
		windowEnd = windowStart + maxPageBtns
		if windowStart < 0 {
			windowStart = 0
			windowEnd = maxPageBtns
		}
		if windowEnd > totalPages {
			windowEnd = totalPages
			windowStart = windowEnd - maxPageBtns
		}
	}

	navButtons := []discord.InteractiveComponent{
		discord.NewSecondaryButton(loc.T("btn.main_menu"), "/speakers/menu"),
	}
	for p := windowStart; p < windowEnd; p++ {
		rangeStart := p*speakersPerPage + 1
		rangeEnd := min((p+1)*speakersPerPage, len(speakers))
		label := fmt.Sprintf("%d-%d", rangeStart, rangeEnd)
		customID := fmt.Sprintf("/speakers/page/%d", p)
		if p == page {
			navButtons = append(navButtons, discord.NewPrimaryButton(label, customID).WithDisabled(true))
		} else {
			navButtons = append(navButtons, discord.NewSecondaryButton(label, customID))
		}
	}
	components = append(components, discord.NewActionRow(navButtons...))

	content := loc.T("setup.speakers_title", "Page", page+1, "Total", totalPages) + "\n"
	if len(speakers) == 0 {
		content += loc.T("setup.no_speakers")
	} else {
		content += loc.T("setup.speakers_count", "Count", len(speakers))
	}
	return content, components
}

// ── Slash command handlers ───────────────────────────────────────────────────

// handleSetup opens the interactive setup panel as an ephemeral message.
// Blocked while a voice raid is active. Authorization handled by withAdmin middleware.
func (h *CommandHandlers) handleSetup(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if h.manager.HasActiveSession(guildID) {
		return e.CreateMessage(ephemeral(loc.T("setup.blocked_active_raid")))
	}

	msg, components := h.buildMainSetupMessage(guildID, loc)
	return e.CreateMessage(discord.MessageCreate{
		Content:    msg,
		Components: components,
		Flags:      discord.MessageFlagEphemeral,
	})
}

// handleStartVoiceRaid starts a new voice raid or joins an existing one as a guest.
// Uses deferred responses so the user gets real feedback on success/failure.
// Authorization handled by withManager middleware.
func (h *CommandHandlers) handleStartVoiceRaid(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	status := h.manager.GetStatus(guildID)
	if status.HasActiveSession() {
		return e.CreateMessage(ephemeral(loc.T("raid.already_active")))
	}

	code, hasCode := data.OptString("code")

	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}

	modeStr, _ := data.OptString("mode")

	if hasCode && code != "" {
		var mode guild.RaidMode
		switch modeStr {
		case callerModeMany:
			mode = guild.RaidModeAllyCaller
		case callerModeOneMany:
			mode = guild.RaidModeOneManyAllyCaller
		default:
			mode = guild.RaidModeAllyListener
		}
		cmdCtx := e.Ctx
		ctx, cancelFunc := context.WithCancel(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(cmdCtx)))
		go func() {
			if warnings := h.manager.CheckGuildChannelAccess(guildID); len(warnings) > 0 {
				cancelFunc()
				h.followUp(e, loc.T("raid.join_blocked_permissions")+formatAccessWarnings(loc, warnings))
				return
			}
			effectiveMode, err := h.manager.JoinSession(ctx, guildID, cancelFunc, mode, code)
			if err != nil {
				cancelFunc()
				slog.WarnContext(cmdCtx, "failed to join relay session", slog.String("code", code), slog.Any("err", err))
				h.followUp(e, loc.T("raid.join_failed", "Code", code, "Err", err.Error()))
				return
			}
			h.followUp(e, loc.T("raid.joined", "Code", code, "Mode", effectiveMode.Pretty(loc)))
		}()
		return nil
	}

	var mode guild.RaidMode
	switch modeStr {
	case callerModeMany:
		mode = guild.RaidModeGuildCaller
	case callerModeOneMany:
		mode = guild.RaidModeOneManyGuildCaller
	default:
		mode = guild.RaidModeOneCaller
	}

	cmdCtx := e.Ctx
	ctx, cancelFunc := context.WithCancel(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(cmdCtx)))
	go func() {
		if warnings := h.manager.CheckGuildChannelAccess(guildID); len(warnings) > 0 {
			cancelFunc()
			h.followUp(e, loc.T("raid.start_blocked_permissions")+formatAccessWarnings(loc, warnings))
			return
		}
		relayCode, err := h.manager.StartVoiceRaid(ctx, guildID, cancelFunc, mode)
		if err != nil {
			cancelFunc()
			slog.WarnContext(cmdCtx, "failed to start voice raid", slog.Any("err", err))
			h.followUp(e, loc.T("raid.start_failed", "Err", err.Error()))
			return
		}
		var msg string
		if relayCode != "" {
			msg = loc.T("raid.started_with_code", "Code", relayCode)
		} else {
			msg = loc.T("raid.started")
		}
		slog.InfoContext(cmdCtx, "voice raid started", slog.String("relayCode", relayCode))
		h.followUp(e, msg)
	}()

	return nil
}

// handleStopVoiceRaid stops the active voice raid.
// Uses a deferred response so the user gets real feedback on success/failure.
// Authorization handled by withManager middleware.
func (h *CommandHandlers) handleStopVoiceRaid(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if status := h.manager.GetStatus(guildID); !status.HasActiveSession() {
		return e.CreateMessage(ephemeral(loc.T("raid.none_active")))
	}

	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}

	cmdCtx := e.Ctx
	go func() {
		if err := h.manager.StopVoiceRaid(cmdCtx, guildID); err != nil {
			slog.WarnContext(cmdCtx, "failed to stop voice raid", slog.String("guildID", guildID.String()), slog.Any("err", err))
			h.followUp(e, loc.T("raid.stop_failed", "Err", err.Error()))
			return
		}
		h.followUp(e, loc.T("raid.stopped"))
	}()

	return nil
}

// handleStatus responds with an ephemeral snapshot of the guild's configuration and
// session state. Authorization handled by withGuild middleware.
func (h *CommandHandlers) handleStatus(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	status := h.manager.GetStatus(guildID)
	return e.CreateMessage(discord.MessageCreate{
		Content: status.Render(loc),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleBindRole sets the capture role directly via the /bind-role slash command.
func (h *CommandHandlers) handleBindRole(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	roleID := data.Role("role").ID
	h.manager.BindRole(guildID, store.RoleTypeCaller, roleID)

	return e.CreateMessage(discord.MessageCreate{
		Content: loc.T("role.caller_set", "RoleID", roleID.String()),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleBindManagerRole sets the manager role directly via the /bind-manager-role slash command.
func (h *CommandHandlers) handleBindManagerRole(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	roleID := data.Role("role").ID
	h.manager.BindRole(guildID, store.RoleTypeManager, roleID)

	return e.CreateMessage(discord.MessageCreate{
		Content: loc.T("role.manager_set", "RoleID", roleID.String()),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// ── Component handlers ───────────────────────────────────────────────────────

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
func (h *CommandHandlers) handleBindRoleMenu(guildID snowflake.ID, loc *i18n.Localizer, data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
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
	if len(values) == 0 || values[0] == "" {
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

// ── Helpers ──────────────────────────────────────────────────────────────────

func requireGuild(loc *i18n.Localizer, guildID *snowflake.ID) (snowflake.ID, *discord.MessageCreate) {
	if guildID == nil {
		msg := ephemeral(loc.T("err.guild_only"))
		return 0, &msg
	}
	return *guildID, nil
}

func ephemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral}
}

func statusEmoji(enabled bool) string {
	if enabled {
		return "🔊"
	}
	return "🔇"
}

// isAdminAuthorized reports whether the member has Administrator permission
// or holds the guild's configured manager role.
func (h *CommandHandlers) isAdminAuthorized(guildID snowflake.ID, member *discord.ResolvedMember) bool {
	if member == nil {
		return false
	}
	if member.Permissions.Has(discord.PermissionAdministrator) {
		return true
	}
	return h.manager.HasManagerRole(guildID, member.Member.RoleIDs)
}

// isManagerAuthorized reports whether the member has Manage Server permission
// or holds the guild's configured manager role.
func (h *CommandHandlers) isManagerAuthorized(guildID snowflake.ID, member *discord.ResolvedMember) bool {
	if member == nil {
		return false
	}
	if member.Permissions.Has(discord.PermissionManageGuild) {
		return true
	}
	return h.manager.HasManagerRole(guildID, member.Member.RoleIDs)
}

// followUp sends an ephemeral follow-up message after a deferred response.
func (h *CommandHandlers) followUp(e *handler.CommandEvent, content string) {
	if _, err := e.CreateFollowupMessage(ephemeral(content)); err != nil {
		slog.Warn("failed to send follow-up message", slog.Any("err", err))
	}
}

// formatAccessWarnings builds a Discord-formatted warning block from the access check
// results. Returns "" when there are no warnings.
func formatAccessWarnings(loc *i18n.Localizer, warnings []manager.ChannelAccessWarning) string {
	if len(warnings) == 0 {
		return ""
	}
	msg := loc.T("permissions.warning_header")
	for _, w := range warnings {
		msg += fmt.Sprintf("\n- <@%s> → <#%s>", w.BotID, w.ChannelID)
	}
	return msg
}

// botPermissions is the Discord permission bitmask required by speaker bots.
// Bit 48 is an unnamed/reserved Discord permission retained from the original
// install URL; remove it if Discord revokes or reassigns the bit.
const botPermissions discord.Permissions = discord.PermissionAddReactions |
	discord.PermissionPrioritySpeaker |
	discord.PermissionSendMessages |
	discord.PermissionSendTTSMessages |
	discord.PermissionUseExternalEmojis |
	discord.PermissionConnect |
	discord.PermissionSpeak |
	discord.PermissionUseVAD |
	discord.PermissionUseApplicationCommands |
	discord.PermissionUseExternalStickers |
	discord.PermissionUseSoundboard |
	discord.PermissionUseExternalSounds |
	discord.PermissionSendVoiceMessages |
	1<<48 // reserved/unnamed bit present in original install URL

func installOwnerURL(clientID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d",
		clientID, botPermissions,
	)
}

func installURL(clientID snowflake.ID, guildID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d&guild_id=%s",
		clientID, botPermissions, guildID,
	)
}
