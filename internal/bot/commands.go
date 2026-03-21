package bot

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
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

	// Modal route
	r.Modal("/speakers/add-modal", h.handleAddSpeakerModal)
}

// ── Slash command handlers ───────────────────────────────────────────────────

func (h *CommandHandlers) handleSetupSpeakers(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	speakers := h.manager.ListSpeakers(guildID)

	var components []discord.LayoutComponent

	// Only show the "Add Speaker" button when the pool still has an unused token.
	if h.manager.HasAvailableToken(guildID) {
		components = append(components, discord.NewActionRow(
			discord.NewSuccessButton("➕ Add Speaker", "/speakers/add"),
		))
	}

	for _, sp := range speakers {
		label := "Enable"
		if sp.Enabled {
			label = "Disable"
		}

		placeholder := "Bind to a voice channel…"
		if sp.BoundChannelID != nil {
			placeholder = fmt.Sprintf("Bound: <#%s>", sp.BoundChannelID)
		}

		components = append(components,
			discord.NewActionRow(
				discord.NewSecondaryButton(
					fmt.Sprintf("%s %s (%s)", statusEmoji(sp.Enabled), sp.Username, label),
					fmt.Sprintf("/speakers/toggle/%s", sp.ID),
				),
			),
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

	if err := h.manager.StartVoiceRaid(context.TODO(), guildID); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: "🔴 **Voice raid started.** All enabled speakers have joined their bound channels.",
	})
}

func (h *CommandHandlers) handleStopVoiceRaid(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	if err := h.manager.StopVoiceRaid(context.TODO(), guildID); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: "⚫ **Voice raid stopped.** All speakers have left their channels.",
	})
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
			enabled = s.Enabled
			break
		}
	}

	if err := h.manager.ToggleSpeaker(context.TODO(), speakerID, !enabled); err != nil {
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

// handleAddSpeakerButton opens a modal to collect a display name for the next pool speaker.
func (h *CommandHandlers) handleAddSpeakerButton(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	// Guard: re-check availability in case it changed since the message was rendered.
	if !h.manager.HasAvailableToken(guildID) {
		return e.CreateMessage(ephemeral("❌ All speaker tokens from the pool have already been added."))
	}

	return e.Modal(discord.ModalCreate{
		CustomID: "/speakers/add-modal",
		Title:    "Add Speaker Bot",
		Components: []discord.LayoutComponent{
			discord.NewLabel("Display Name",
				discord.NewShortTextInput("username").
					WithPlaceholder("Friendly name for this speaker (e.g. speaker-1)").
					WithRequired(true),
			),
		},
	})
}

// handleBindChannel updates the voice channel bound to a speaker.
func (h *CommandHandlers) handleBindChannel(data discord.SelectMenuInteractionData, e *handler.ComponentEvent) error {
	speakerID, err := snowflake.Parse(e.Vars["speakerID"])
	if err != nil {
		return e.CreateMessage(ephemeral("invalid speaker ID"))
	}

	channelData, ok := data.(discord.ChannelSelectMenuInteractionData)
	if !ok {
		return e.CreateMessage(ephemeral("unexpected interaction data type"))
	}

	channels := channelData.Channels()
	if len(channels) == 0 {
		h.manager.UnbindChannel(speakerID)
		return e.CreateMessage(discord.MessageCreate{
			Content: "✅ Channel binding removed.",
			Flags:   discord.MessageFlagEphemeral,
		})
	}

	channelID := channels[0].ID
	if err := h.manager.BindChannel(speakerID, channelID); err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Speaker `%s` bound to <#%s>.", speakerID, channelID),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// handleAddSpeakerModal processes the modal submission — picks the next pool token automatically.
func (h *CommandHandlers) handleAddSpeakerModal(e *handler.ModalEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	username, _ := e.Data.OptText("username")
	if username == "" {
		return e.CreateMessage(ephemeral("❌ Display name is required."))
	}

	sp, err := h.manager.AddNextSpeaker(context.TODO(), guildID, username)
	if err != nil {
		return e.CreateMessage(ephemeral("❌ " + err.Error()))
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Speaker **%s** (`%s`) added and connected.", sp.Username, sp.ID),
		Flags:   discord.MessageFlagEphemeral,
	})
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
