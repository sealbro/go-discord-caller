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
	"github.com/sealbro/go-discord-caller/internal/manager"
)

// Commands is the list of slash commands registered with Discord.
var Commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:                     "setup",
		Description:              "List and configure all speaker bots in this server",
		DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
	},
	discord.SlashCommandCreate{
		Name:                     "start",
		Description:              "Make all enabled speakers join their bound voice channels",
		DefaultMemberPermissions: permPtr(discord.PermissionManageGuild),
	},
	discord.SlashCommandCreate{
		Name:                     "stop",
		Description:              "Make all active speakers leave their voice channels",
		DefaultMemberPermissions: permPtr(discord.PermissionManageGuild),
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
}

// permPtr wraps a Permissions value into the omit.Omit[*discord.Permissions] type
// required by SlashCommandCreate.DefaultMemberPermissions.
func permPtr(p discord.Permissions) omit.Omit[*discord.Permissions] {
	return omit.New(&p)
}

// CommandHandlers wires all slash command and component routes to the manager service.
type CommandHandlers struct {
	manager *manager.Service
}

// NewCommandHandlers creates a new CommandHandlers.
func NewCommandHandlers(m *manager.Service) *CommandHandlers {
	return &CommandHandlers{manager: m}
}

// Register attaches all routes to the given router.
func (h *CommandHandlers) Register(r handler.Router) {
	r.SlashCommand("/setup", h.handleSetup)
	r.SlashCommand("/start", h.handleStartVoiceRaid)
	r.SlashCommand("/stop", h.handleStopVoiceRaid)
	r.SlashCommand("/status", h.handleStatus)
	r.SlashCommand("/bind-role", h.handleBindRole)

	// Main setup menu components
	r.SelectMenuComponent("/setup/bind-role", h.handleBindRoleMenu)
	r.SelectMenuComponent("/owner/bind-channel", h.handleBindOwnerChannel)
	r.ButtonComponent("/speakers/page/{page}", h.handleSpeakersPage)
	r.ButtonComponent("/speakers/menu", h.handleMainMenu)

	// Speaker page components (page number is embedded in the custom ID)
	r.ButtonComponent("/speakers/toggle/{speakerID}/{page}", h.handleToggleSpeaker)
	r.ButtonComponent("/speakers/add", h.handleAddSpeakerButton)
	r.SelectMenuComponent("/speakers/bind-channel/{speakerID}/{page}", h.handleBindChannel)
}

// ── Constants ─────────────────────────────────────────────────────────────────

// speakersPerPage is the maximum number of speakers shown per page in the
// speaker bind menu.  Discord allows 5 action rows per message:
//   - row 1 = toggle buttons
//   - rows 2-4 = channel-select menus (one per speaker)
//   - row 5 = navigation buttons
const speakersPerPage = 3

// ── Setup message builders ────────────────────────────────────────────────────

// buildMainSetupMessage builds the main setup message:
//   - Row 1: role select (combobox)
//   - Row 2: owner channel select (combobox)
//   - Row 3: "Bind Speakers" button
func (h *CommandHandlers) buildMainSetupMessage(guildID snowflake.ID) (string, []discord.LayoutComponent) {
	status := h.manager.GetStatus(guildID)
	var components []discord.LayoutComponent

	// Row 1 — role selector
	roleMenu := discord.NewRoleSelectMenu("/setup/bind-role", "Select capture role…")
	if status.RoleID != nil {
		roleMenu = roleMenu.AddDefaultValue(*status.RoleID)
	}
	components = append(components, discord.NewActionRow(roleMenu))

	// Row 2 — owner bot channel selector
	ownerMenu := discord.NewChannelSelectMenu("/owner/bind-channel", "Bind caller bot to a voice channel…").
		WithChannelTypes(discord.ChannelTypeGuildVoice)
	if chID, ok := h.manager.GetOwnerChannel(guildID); ok {
		ownerMenu = ownerMenu.AddDefaultValue(chID)
	}
	components = append(components, discord.NewActionRow(ownerMenu))

	// Row 3 — "Bind Speakers" button
	components = append(components, discord.NewActionRow(
		discord.NewPrimaryButton("⚙️ Bind Speakers", "/speakers/page/0"),
	))

	return "**Speaker Setup**\n" + status.String(), components
}

// buildSpeakersPageMessage builds a paginated speaker bind page.
//
// Layout (≤5 rows):
//   - Row 1: toggle buttons for each speaker + "Add Speaker" on last page (if token available)
//   - Rows 2-4: voice-channel select menu per speaker (up to speakersPerPage)
//   - Row 5: [◀◀ Prev] [🏠 Main Menu] [Next ▶▶]
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

	// Row 1 — toggle buttons (+ "Add Speaker" on last page when a token is available)
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
	isLastPage := page == totalPages-1
	if isLastPage && h.manager.HasAvailableToken(guildID) {
		buttons = append(buttons, discord.NewSuccessButton("➕ Add Speaker", "/speakers/add"))
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

	// Row 5 — navigation
	prevBtn := discord.NewSecondaryButton("◀◀ Prev", fmt.Sprintf("/speakers/page/%d", page-1)).
		WithDisabled(page == 0)
	mainBtn := discord.NewSecondaryButton("🏠 Main Menu", "/speakers/menu")
	nextBtn := discord.NewSecondaryButton("Next ▶▶", fmt.Sprintf("/speakers/page/%d", page+1)).
		WithDisabled(page >= totalPages-1)
	components = append(components, discord.NewActionRow(prevBtn, mainBtn, nextBtn))

	content := fmt.Sprintf("**Speaker Bindings** — Page %d/%d\n", page+1, totalPages)
	if len(speakers) == 0 {
		content += "_No speakers registered yet._"
	} else {
		content += fmt.Sprintf("_%d speaker(s) total._", len(speakers))
	}
	return content, components
}

// ── Slash command handlers ───────────────────────────────────────────────────

func (h *CommandHandlers) handleSetup(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
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

func (h *CommandHandlers) handleStartVoiceRaid(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	status := h.manager.GetStatus(guildID)
	if status.HasActiveSession() {
		return e.CreateMessage(ephemeral("⚠️ A voice raid is already active in this server."))
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	go func() {
		if err := h.manager.StartVoiceRaid(ctx, cancelFunc, guildID); err != nil {
			cancelFunc()
			slog.Warn("failed to start voice raid", slog.Any("err", err))
		}
	}()

	return e.CreateMessage(ephemeral("🔴 **Voice raid started.** All enabled speakers have joined their bound channels."))
}

func (h *CommandHandlers) handleStopVoiceRaid(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	if status := h.manager.GetStatus(guildID); !status.HasActiveSession() {
		return e.CreateMessage(ephemeral("⚠️ There is no active voice raid in this server."))
	}

	go func() {
		if err := h.manager.StopVoiceRaid(context.TODO(), guildID); err != nil {
			slog.Warn("failed to stop voice raid", slog.String("guildID", guildID.String()), slog.Any("err", err))
		}
	}()

	return e.CreateMessage(ephemeral("⚫ **Voice raid stopped.** All speakers have left their channels."))
}

func (h *CommandHandlers) handleStatus(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	status := h.manager.GetStatus(guildID)
	return e.CreateMessage(discord.MessageCreate{
		Content: status.String(),
		Flags:   discord.MessageFlagEphemeral,
	})
}

func (h *CommandHandlers) handleBindRole(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	roleID := data.Role("role").ID
	h.manager.BindRole(guildID, roleID)

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Capture role set to <@&%s>. Only members with this role will be relayed.", roleID),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// ── Component handlers ───────────────────────────────────────────────────────

// handleSpeakersPage opens (or navigates to) a speaker bind page.
func (h *CommandHandlers) handleSpeakersPage(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	page, _ := strconv.Atoi(e.Vars["page"])

	msg, components := h.buildSpeakersPageMessage(guildID, page)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleMainMenu returns the user to the main setup message.
func (h *CommandHandlers) handleMainMenu(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	msg, components := h.buildMainSetupMessage(guildID)
	return e.UpdateMessage(discord.NewMessageUpdate().
		WithContent(msg).
		WithComponents(components...))
}

// handleBindRoleMenu handles role selection from the setup message and refreshes it.
func (h *CommandHandlers) handleBindRoleMenu(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	roleData, ok := data.(discord.RoleSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	roles := roleData.Roles()
	if len(roles) == 0 {
		return e.CreateMessage(ephemeral("❌ No role selected."))
	}

	h.manager.BindRole(guildID, roles[0].ID)

	msg, components := h.buildMainSetupMessage(guildID)
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

	page, _ := strconv.Atoi(e.Vars["page"])

	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
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

// handleAddSpeakerButton resolves the next pool bot's ApplicationID, builds a
// Discord OAuth2 invite URL pre-targeted at the current guild, and shows only
// the invite link. The bot is registered automatically once it joins the server
// via the GuildMemberJoin event listener.
func (h *CommandHandlers) handleAddSpeakerButton(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	botUserID, ok := h.manager.NextSpeakerID(guildID)
	if !ok {
		return e.CreateMessage(ephemeral("❌ All speaker tokens from the pool have already been added."))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: "**Add Speaker Bot**\n" +
			"1. Click **Invite to Server** — the bot will be pre-selected for this server.\n" +
			"2. Complete the authorisation in the browser.\n" +
			"3. The bot will be registered automatically once it joins the server.",
		Components: []discord.LayoutComponent{
			discord.NewActionRow(
				discord.NewLinkButton("🔗 Invite to Server", installURL(botUserID, guildID)),
			),
		},
		Flags: discord.MessageFlagEphemeral,
	})
}

// handleBindChannel updates the voice channel bound to a speaker and refreshes the speaker page.
func (h *CommandHandlers) handleBindChannel(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

	page, _ := strconv.Atoi(e.Vars["page"])

	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
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
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
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

func requireGuild(guildID *snowflake.ID) (snowflake.ID, error) {
	if guildID == nil {
		return 0, fmt.Errorf("this command can only be used inside a server")
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

func installURL(clientID snowflake.ID, guildID snowflake.ID) string {
	permissions := "391565762894144"
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%s&guild_id=%s",
		clientID, permissions, guildID,
	)
}
