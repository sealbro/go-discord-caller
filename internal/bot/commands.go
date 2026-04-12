package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// Commands is the list of slash commands registered with Discord.
var Commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "setup",
		Description: "List and configure all speaker bots in this server",
	},
	discord.SlashCommandCreate{
		Name:        "start",
		Description: "Start a voice raid, or join an existing one as a guest using a relay code",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "code",
				Description: "Relay code from another server's active voice raid (leave empty to start a new one)",
				Required:    false,
			},
			discord.ApplicationCommandOptionString{
				Name:        "mode",
				Description: "One Caller: only owner channel captures. Many Callers: all channels capture and mix. (default: One Caller)",
				Required:    false,
				Choices: []discord.ApplicationCommandOptionChoiceString{
					{Name: "One Caller", Value: string(callerModeOne)},
					{Name: "Many Callers", Value: string(callerModeMany)},
				},
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "stop",
		Description: "Make all active speakers leave their voice channels",
	},
	discord.SlashCommandCreate{
		Name:        "status",
		Description: "Show current speaker bindings and voice raid state",
	},
	discord.SlashCommandCreate{
		Name:                     "bind-role",
		Description:              "Set the role whose members' voice will be captured and relayed",
		DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionRole{
				Name:        "role",
				Description: "The role to capture voice from",
				Required:    true,
			},
		},
	},
	discord.SlashCommandCreate{
		Name:                     "bind-manager-role",
		Description:              "Set the role whose members are allowed to setup, start and stop the bot",
		DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionRole{
				Name:        "role",
				Description: "The manager role",
				Required:    true,
			},
		},
	},
}

// permPtr wraps a Permissions value into the omit.Omit[*discord.Permissions] type
// required by SlashCommandCreate.DefaultMemberPermissions.
func permPtr(p discord.Permissions) omit.Omit[*discord.Permissions] {
	return omit.New(&p)
}

// CommandHandlers wires all slash command and component routes to the manager service.
type CommandHandlers struct {
	manager ManagerService
}

// NewCommandHandlers creates a new CommandHandlers.
func NewCommandHandlers(m ManagerService) *CommandHandlers {
	return &CommandHandlers{manager: m}
}

// Register attaches all routes to the given router.
func (h *CommandHandlers) Register(r handler.Router) {
	r.SlashCommand("/setup", h.handleSetup)
	r.SlashCommand("/start", h.handleStartVoiceRaid)
	r.SlashCommand("/stop", h.handleStopVoiceRaid)
	r.SlashCommand("/status", h.handleStatus)
	r.SlashCommand("/bind-role", h.handleBindRole)
	r.SlashCommand("/bind-manager-role", h.handleBindManagerRole)

	// Main setup menu components
	r.SelectMenuComponent("/setup/bind-role", h.handleBindRoleMenu)
	r.SelectMenuComponent("/setup/bind-manager-role", h.handleBindManagerRoleMenu)
	r.SelectMenuComponent("/owner/bind-channel", h.handleBindOwnerChannel)
	r.ButtonComponent("/roles/menu", h.handleRolesMenu)
	r.ButtonComponent("/speakers/page/{page}", h.handleSpeakersPage)
	r.ButtonComponent("/speakers/menu", h.handleMainMenu)

	// Speaker page components (page number is embedded in the custom ID)
	r.ButtonComponent("/speakers/toggle/{speakerID}/{page}", h.handleToggleSpeaker)
	r.ButtonComponent("/speakers/add", h.handleAddSpeakerButton)
	r.SelectMenuComponent("/speakers/bind-channel/{speakerID}/{page}", h.handleBindChannel)
}

// ── Constants ─────────────────────────────────────────────────────────────────

// callerModeChoice is the value of the "mode" slash command option.
type callerModeChoice string

const (
	callerModeOne  callerModeChoice = "one"
	callerModeMany callerModeChoice = "many"
)

// speakersPerPage is the maximum number of speakers shown per page in the
// speaker bind menu.  Discord allows 5 action rows per message:
//   - row 1 = toggle buttons
//   - rows 2-4 = channel-select menus (one per speaker)
//   - row 5 = navigation buttons
const speakersPerPage = 3

// ── Setup message builders ────────────────────────────────────────────────────

// buildMainSetupMessage builds the main setup message.
//
// Layout:
//   - Row 1: owner voice-channel select menu
//   - Row 2: "🎭 Bind Roles", "⚙️ Bind Speakers" buttons; "➕ Add Speaker" appended when an uninvited pool bot is available
func (h *CommandHandlers) buildMainSetupMessage(guildID snowflake.ID) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	var components []discord.LayoutComponent

	// Row 1 — owner bot channel selector
	ownerMenu := discord.NewChannelSelectMenu("/owner/bind-channel", "Bind caller bot to a voice channel…").
		WithChannelTypes(discord.ChannelTypeGuildVoice)
	if chID, ok := h.manager.GetOwnerChannel(guildID); ok {
		ownerMenu = ownerMenu.AddDefaultValue(chID)
	}
	components = append(components, discord.NewActionRow(ownerMenu))

	// Row 2 — action buttons
	buttons := []discord.InteractiveComponent{
		discord.NewPrimaryButton("🎭 Bind Roles", "/roles/menu"),
		discord.NewPrimaryButton("⚙️ Bind Speakers", "/speakers/page/0"),
	}
	if h.manager.HasAvailableToken(guildID) {
		buttons = append(buttons, discord.NewSuccessButton("➕ Add Speaker", "/speakers/add"))
	}
	components = append(components, discord.NewActionRow(buttons...))

	return "**Speaker Setup**\n" + status.String(), components
}

// buildRolesPageMessage builds the "Bind Roles" sub-page.
//
// Layout:
//   - Row 1: capture role select menu (pre-filled with current binding)
//   - Row 2: manager role select menu (pre-filled with current binding)
//   - Row 3: "🏠 Main Menu" button
func (h *CommandHandlers) buildRolesPageMessage(guildID snowflake.ID) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	var components []discord.LayoutComponent

	// Row 1 — capture role selector
	roleMenu := discord.NewRoleSelectMenu("/setup/bind-role", "Select capture role…")
	if status.CallerRoleID != nil {
		roleMenu = roleMenu.AddDefaultValue(*status.CallerRoleID)
	}
	components = append(components, discord.NewActionRow(roleMenu))

	// Row 2 — manager role selector
	managerRoleMenu := discord.NewRoleSelectMenu("/setup/bind-manager-role", "Select manager role…")
	if status.ManagerRoleID != nil {
		managerRoleMenu = managerRoleMenu.AddDefaultValue(*status.ManagerRoleID)
	}
	components = append(components, discord.NewActionRow(managerRoleMenu))

	// Row 3 — navigation
	components = append(components, discord.NewActionRow(
		discord.NewSecondaryButton("🏠 Main Menu", "/speakers/menu"),
	))

	return "**Role Bindings**\n" + status.String(), components
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
func (h *CommandHandlers) buildSpeakersPageMessage(guildID snowflake.ID, page int) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	speakers := status.GetSortedSpeakers()

	totalPages := (len(speakers) + speakersPerPage - 1) / speakersPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	if page < 0 {
		page = 0
	}

	start := page * speakersPerPage
	end := start + speakersPerPage
	if end > len(speakers) {
		end = len(speakers)
	}
	pageSpeakers := speakers[start:end]

	var components []discord.LayoutComponent

	// Row 1 — toggle buttons
	var buttons []discord.InteractiveComponent
	for _, sp := range pageSpeakers {
		label := "Enable"
		if sp.Enabled {
			label = "Disable"
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
			fmt.Sprintf("Bind %s to a voice channel…", sp.Username),
		).WithChannelTypes(discord.ChannelTypeGuildVoice)
		if chID, ok := h.manager.GetBoundChannel(guildID, sp.ID); ok {
			spMenu = spMenu.AddDefaultValue(chID)
		}
		components = append(components, discord.NewActionRow(spMenu))
	}

	// Row 5 — navigation: [🏠 Main Menu] + up to 4 page-range jump buttons.
	// The current page's button is primary+disabled; others are secondary.
	// When totalPages > 4 a sliding window of 4 is centered on the current page.
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
		discord.NewSecondaryButton("🏠 Main Menu", "/speakers/menu"),
	}
	for p := windowStart; p < windowEnd; p++ {
		rangeStart := p*speakersPerPage + 1
		rangeEnd := (p + 1) * speakersPerPage
		if rangeEnd > len(speakers) {
			rangeEnd = len(speakers)
		}
		label := fmt.Sprintf("%d-%d", rangeStart, rangeEnd)
		customID := fmt.Sprintf("/speakers/page/%d", p)
		if p == page {
			navButtons = append(navButtons, discord.NewPrimaryButton(label, customID).WithDisabled(true))
		} else {
			navButtons = append(navButtons, discord.NewSecondaryButton(label, customID))
		}
	}
	components = append(components, discord.NewActionRow(navButtons...))

	content := fmt.Sprintf("**Speaker Bindings** — Page %d/%d\n", page+1, totalPages)
	if len(speakers) == 0 {
		content += "_No speakers registered yet._"
	} else {
		content += fmt.Sprintf("_%d speaker(s) total._", len(speakers))
	}
	return content, components
}

// ── Slash command handlers ───────────────────────────────────────────────────

// handleSetup opens the interactive setup panel as an ephemeral message.
// Requires Administrator permission or the configured manager role.
// Blocked while a voice raid is active.
func (h *CommandHandlers) handleSetup(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	if !h.isAdminAuthorized(guildID, e.Member()) {
		return e.CreateMessage(ephemeral("❌ You need the Administrator permission or the server's manager role to use this command."))
	}

	if h.manager.HasActiveSession(guildID) {
		return e.CreateMessage(ephemeral("⚠️ Setup is not available while a voice raid is active. Stop the raid first."))
	}

	msg, components := h.buildMainSetupMessage(guildID)
	return e.CreateMessage(discord.MessageCreate{
		Content:    msg,
		Components: components,
		Flags:      discord.MessageFlagEphemeral,
	})
}

// handleStartVoiceRaid starts a new voice raid or joins an existing one as a guest.
//
// Without code: calls StartVoiceRaid — owner bot joins its bound channel, all enabled
// speakers join theirs, and the relay session begins. The relay code is visible in /status.
//
// With code: calls JoinSession — owner bot and speakers join as a guest of the host guild
// identified by the relay code; session tears down when the host stops or this guild cancels.
//
// Both operations are async (goroutine); failures are logged only — the initial Discord
// response is sent before the goroutine completes, so the user always sees the "started"
// message regardless of whether the underlying operation succeeds.
func (h *CommandHandlers) handleStartVoiceRaid(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	if !h.isManagerAuthorized(guildID, e.Member()) {
		return e.CreateMessage(ephemeral("❌ You need the Manage Server permission or the server's manager role to use this command."))
	}

	status := h.manager.GetStatus(guildID)
	if status.HasActiveSession() {
		return e.CreateMessage(ephemeral("⚠️ A voice raid is already active in this server."))
	}

	code, hasCode := data.OptString("code")
	manyCallers := false
	if modeStr, ok := data.OptString("mode"); ok {
		manyCallers = callerModeChoice(modeStr) == callerModeMany
	}

	if hasCode && code != "" {
		mode := domain.RaidModeGuestOne
		if manyCallers {
			mode = domain.RaidModeAllyCaller
		}
		ctx, cancelFunc := context.WithCancel(context.Background())
		go func() {
			if err := h.manager.JoinSession(ctx, cancelFunc, code, guildID, mode); err != nil {
				cancelFunc()
				slog.Warn("failed to join relay session", slog.String("code", code), slog.Any("err", err))
			}
		}()
		return e.CreateMessage(ephemeral(fmt.Sprintf("🔴 **Joined relay session** `%s`. Speakers are connecting to their bound channels.", code)))
	}

	mode := domain.RaidModeOneCaller
	if manyCallers {
		mode = domain.RaidModeGuildCaller
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	go func() {
		relayCode, err := h.manager.StartVoiceRaid(ctx, cancelFunc, guildID, mode)
		if err != nil {
			cancelFunc()
			slog.Warn("failed to start voice raid", slog.Any("err", err))
		} else {
			slog.Info("voice raid started", slog.String("relayCode", relayCode))
		}
	}()

	return e.CreateMessage(ephemeral("🔴 **Voice raid started.** All enabled speakers have joined their bound channels. Use `/status` to see the relay code."))
}

// handleStopVoiceRaid stops the active voice raid: cancels the relay goroutine,
// makes all speakers leave their channels, and disconnects the owner bot.
// For a host guild this also closes the relay session, signalling all guests to tear down.
func (h *CommandHandlers) handleStopVoiceRaid(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	if !h.isManagerAuthorized(guildID, e.Member()) {
		return e.CreateMessage(ephemeral("❌ You need the Manage Server permission or the server's manager role to use this command."))
	}

	if status := h.manager.GetStatus(guildID); !status.HasActiveSession() {
		return e.CreateMessage(ephemeral("⚠️ There is no active voice raid in this server."))
	}

	go func() {
		if err := h.manager.StopVoiceRaid(context.Background(), guildID); err != nil {
			slog.Warn("failed to stop voice raid", slog.String("guildID", guildID.String()), slog.Any("err", err))
		}
	}()

	return e.CreateMessage(ephemeral("⚫ **Voice raid stopped.** All speakers have left their channels."))
}

// handleStatus responds with an ephemeral snapshot of the guild's configuration and
// session state: roles, channel bindings, relay code, active session, and guest guild names.
func (h *CommandHandlers) handleStatus(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	status := h.manager.GetStatus(guildID)
	return e.CreateMessage(discord.MessageCreate{
		Content: status.String(),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleBindRole sets the capture role directly via the /bind-role slash command.
func (h *CommandHandlers) handleBindRole(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	roleID := data.Role("role").ID
	h.manager.BindCallerRole(guildID, roleID)

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Capture role set to <@&%s>. Only members with this role will be relayed.", roleID),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleBindManagerRole sets the manager role directly via the /bind-manager-role slash command.
func (h *CommandHandlers) handleBindManagerRole(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	roleID := data.Role("role").ID
	h.manager.BindManagerRole(guildID, roleID)

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Manager role set to <@&%s>. Members with this role can setup, start and stop the bot.", roleID),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// ── Component handlers ───────────────────────────────────────────────────────

// handleSpeakersPage opens (or navigates to) a speaker bind page.
func (h *CommandHandlers) handleSpeakersPage(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.Warn("handleSpeakersPage: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	msg, components := h.buildSpeakersPageMessage(guildID, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleRolesMenu opens the roles bind page.
func (h *CommandHandlers) handleRolesMenu(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	msg, components := h.buildRolesPageMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleMainMenu returns the user to the main setup message.
func (h *CommandHandlers) handleMainMenu(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	msg, components := h.buildMainSetupMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindRoleMenu handles capture role selection from the setup message and refreshes it.
func (h *CommandHandlers) handleBindRoleMenu(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	roleData, ok := data.(discord.RoleSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	roles := roleData.Roles()
	if len(roles) == 0 {
		return e.CreateMessage(ephemeral("❌ No role selected."))
	}

	h.manager.BindCallerRole(guildID, roles[0].ID)

	msg, components := h.buildRolesPageMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindManagerRoleMenu handles manager role selection from the roles page and refreshes it.
func (h *CommandHandlers) handleBindManagerRoleMenu(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	roleData, ok := data.(discord.RoleSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	roles := roleData.Roles()
	if len(roles) == 0 {
		return e.CreateMessage(ephemeral("❌ No role selected."))
	}

	h.manager.BindManagerRole(guildID, roles[0].ID)

	msg, components := h.buildRolesPageMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleToggleSpeaker enables or disables a speaker and refreshes the speaker page.
func (h *CommandHandlers) handleToggleSpeaker(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.Warn("handleToggleSpeaker: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	status := h.manager.GetStatus(guildID)
	sp, ok := status.Speakers[speakerID]
	if !ok {
		return e.CreateMessage(ephemeral("❌ Speaker not found in this guild."))
	}

	if err := h.manager.ToggleSpeaker(guildID, speakerID, !sp.Enabled); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	msg, components := h.buildSpeakersPageMessage(guildID, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleAddSpeakerButton navigates to the "Add Speaker" sub-page.
// It resolves the next uninvited pool bot, builds a Discord OAuth2 invite URL
// pre-targeted at this guild, and shows a link button alongside a "🏠 Main Menu" return.
// The bot is registered automatically via the GuildMemberJoin event once it accepts the invite.
func (h *CommandHandlers) handleAddSpeakerButton(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	botUserID, ok := h.manager.NextSpeakerID(guildID)
	if !ok {
		return e.CreateMessage(ephemeral("❌ All speaker tokens from the pool have already been added."))
	}

	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent("**Add Speaker Bot**\n"+
			"1. Click **Invite to Server** — the bot will be pre-selected for this server.\n"+
			"2. Complete the authorisation in the browser.\n"+
			"3. The bot will be registered automatically once it joins the server.").
		WithComponents(
			discord.NewActionRow(
				discord.NewLinkButton("🔗 Invite to Server", installURL(botUserID, guildID)),
			),
			discord.NewActionRow(
				discord.NewSecondaryButton("🏠 Main Menu", "/speakers/menu"),
			),
		))
}

// handleBindChannel updates the voice channel bound to a speaker and refreshes the speaker page.
func (h *CommandHandlers) handleBindChannel(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

	page, err := strconv.Atoi(e.Vars["page"])
	if err != nil {
		slog.Warn("handleBindChannel: invalid page number", slog.String("page", e.Vars["page"]), slog.Any("err", err))
		page = 0
	}

	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	channelData, ok := data.(discord.ChannelSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	channels := channelData.Channels()
	if len(channels) == 0 {
		h.manager.UnbindChannel(guildID, speakerID)
	} else {
		h.manager.BindChannel(guildID, speakerID, channels[0].ID)
	}

	msg, components := h.buildSpeakersPageMessage(guildID, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindOwnerChannel updates the owner bot's voice channel and refreshes the main setup message.
func (h *CommandHandlers) handleBindOwnerChannel(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	guildID, errMsg := requireGuild(e.GuildID())
	if errMsg != nil {
		return e.CreateMessage(*errMsg)
	}

	channelData, ok := data.(discord.ChannelSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	channels := channelData.Channels()
	if len(channels) == 0 {
		h.manager.UnbindOwnerChannel(guildID)
	} else {
		h.manager.BindOwnerChannel(guildID, channels[0].ID)
	}

	msg, components := h.buildMainSetupMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func requireGuild(guildID *snowflake.ID) (snowflake.ID, *discord.MessageCreate) {
	if guildID == nil {
		return 0, new(ephemeral("this command can only be used inside a server"))
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

func installOwnerURL(clientID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=391565762894144",
		clientID,
	)
}

func installURL(clientID snowflake.ID, guildID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=391565762894144&guild_id=%s",
		clientID, guildID,
	)
}
