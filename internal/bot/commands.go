package bot

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

// Commands is the list of slash commands registered with Discord.
var Commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:                     "setup-speakers",
		Description:              "List and configure all speaker bots in this server",
		DefaultMemberPermissions: permPtr(discord.PermissionAdministrator),
	},
	discord.SlashCommandCreate{
		Name:                     "start-voice-raid",
		Description:              "Make all enabled speakers join their bound voice channels",
		DefaultMemberPermissions: permPtr(discord.PermissionManageGuild),
	},
	discord.SlashCommandCreate{
		Name:                     "stop-voice-raid",
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
	r.SlashCommand("/setup-speakers", h.handleSetupSpeakers)
	r.SlashCommand("/start-voice-raid", h.handleStartVoiceRaid)
	r.SlashCommand("/stop-voice-raid", h.handleStopVoiceRaid)
	r.SlashCommand("/status", h.handleStatus)
	r.SlashCommand("/bind-role", h.handleBindRole)

	// Component routes
	r.ButtonComponent("/speakers/toggle/{speakerID}", h.handleToggleSpeaker)
	r.ButtonComponent("/speakers/add", h.handleAddSpeakerButton)
	r.SelectMenuComponent("/speakers/bind-channel/{speakerID}", h.handleBindChannel)
	r.SelectMenuComponent("/owner/bind-channel", h.handleBindOwnerChannel)
}

// ── Slash command handlers ───────────────────────────────────────────────────

func (h *CommandHandlers) handleSetupSpeakers(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	speakers := h.manager.ListSpeakers(guildID)

	var components []discord.LayoutComponent

	// Row 1 — owner bot channel selector
	ownerPlaceholder := "Bind caller bot to a voice channel…"
	if chID, ok := h.manager.GetOwnerChannel(guildID); ok {
		ownerPlaceholder = fmt.Sprintf("Owner bot channel: <#%s>", chID)
	}
	components = append(components,
		discord.NewActionRow(
			discord.NewChannelSelectMenu("/owner/bind-channel", ownerPlaceholder).
				WithChannelTypes(discord.ChannelTypeGuildVoice),
		),
	)

	// Discord limit: 5 action rows per message.
	// Layout: row 1 = owner select, row 2 = all buttons, rows 3-5 = channel selects → max 3 speakers shown.
	const maxSpeakersShown = 3

	shown := speakers
	if len(shown) > maxSpeakersShown {
		shown = shown[:maxSpeakersShown]
	}

	// Row 2 — "Add Speaker" + one toggle button per shown speaker (all in one row, ≤5 buttons).
	var buttons []discord.InteractiveComponent
	if h.manager.HasAvailableToken(guildID) {
		buttons = append(buttons, discord.NewSuccessButton("➕ Add Speaker", "/speakers/add"))
	}

	buildButton := func(sp *domain.Speaker) discord.InteractiveComponent {
		membership, _ := sp.Guilds[guildID]
		button := discord.NewSecondaryButton(
			fmt.Sprintf("%s %s", sp.Username, statusEmoji(membership.Enabled)),
			fmt.Sprintf("/speakers/toggle/%s", sp.ID),
		)
		return button
	}

	for _, sp := range shown {
		buttons = append(buttons, buildButton(sp))
	}

	if len(buttons) > 0 {
		components = append(components, discord.NewActionRow(buttons...))
	}

	// Rows 3-5 — one channel select per shown speaker.
	for _, sp := range shown {
		placeholder := fmt.Sprintf("Bind %s to a voice channel…", sp.Username)
		if chID, ok := h.manager.GetBoundChannel(sp.ID, guildID); ok {
			placeholder = fmt.Sprintf("<@%s> → <#%s>", sp.ID, chID)
		}
		components = append(components,
			discord.NewActionRow(
				discord.NewChannelSelectMenu(
					fmt.Sprintf("/speakers/bind-channel/%s", sp.ID),
					placeholder,
				).WithChannelTypes(discord.ChannelTypeGuildVoice),
			),
		)
	}

	msg := "**Speaker Setup**\n"
	if len(speakers) == 0 {
		msg += "_No speakers registered yet. Use **Add Speaker** to register one._"
	} else {
		msg += fmt.Sprintf("_%d speaker(s) registered._", len(speakers))
		if len(speakers) > maxSpeakersShown {
			msg += fmt.Sprintf(" _(showing first %d)_", maxSpeakersShown)
		}
	}

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

	// Reject immediately if a raid is already running in this guild.
	if status := h.manager.GetStatus(guildID); status.Session != nil && status.Session.Active {
		return e.CreateMessage(ephemeral("⚠️ A voice raid is already active in this server."))
	}

	go func() {
		if err = h.manager.StartVoiceRaid(context.TODO(), guildID); err != nil {
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

	// Reject immediately if there is no active raid in this guild.
	if status := h.manager.GetStatus(guildID); status.Session == nil || !status.Session.Active {
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

// handleToggleSpeaker enables or disables a speaker when the toggle button is clicked.
func (h *CommandHandlers) handleToggleSpeaker(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	// Resolve current enabled state.
	status := h.manager.GetStatus(guildID)
	var enabled bool
	for _, s := range status.Speakers {
		if s.ID == speakerID {
			if membership, ok := s.Guilds[guildID]; ok {
				enabled = membership.Enabled
			}
			break
		}
	}

	if err := h.manager.ToggleSpeaker(context.TODO(), speakerID, guildID, !enabled); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	action := "enabled"
	if enabled {
		action = "disabled"
	}
	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Speaker `%s` %s.", speakerID, action),
		Flags:   discord.MessageFlagEphemeral,
	})
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

	installURL := installUrl(botUserID, guildID)

	return e.CreateMessage(discord.MessageCreate{
		Content: "**Add Speaker Bot**\n" +
			"1. Click **Invite to Server** — the bot will be pre-selected for this server.\n" +
			"2. Complete the authorisation in the browser.\n" +
			"3. The bot will be registered automatically once it joins the server.",
		Components: []discord.LayoutComponent{
			discord.NewActionRow(
				discord.NewLinkButton("🔗 Invite to Server", installURL),
			),
		},
		Flags: discord.MessageFlagEphemeral,
	})
}

// handleBindChannel updates the voice channel bound to a speaker.
func (h *CommandHandlers) handleBindChannel(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

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
		h.manager.UnbindChannel(speakerID, guildID)
		return e.CreateMessage(discord.MessageCreate{
			Content: "✅ Channel binding removed.",
			Flags:   discord.MessageFlagEphemeral,
		})
	}

	channelID := channels[0].ID
	if err := h.manager.BindChannel(speakerID, guildID, channelID); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Speaker <@%s> bound to <#%s>.", speakerID, channelID),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleBindOwnerChannel updates the voice channel the owner bot will join.
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
		return e.CreateMessage(ephemeral("✅ Owner bot channel binding removed."))
	}

	channelID := channels[0].ID
	h.manager.BindOwnerChannel(guildID, channelID)

	return e.CreateMessage(ephemeral(fmt.Sprintf("✅ Owner bot will join <#%s> during voice raids.", channelID)))
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
		return "✅"
	}
	return "❌"
}

func installUrl(clientID snowflake.ID, guildID snowflake.ID) string {
	permissions := "391565762894144"
	installURL := fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%s&guild_id=%s",
		clientID, permissions, guildID,
	)
	return installURL
}
