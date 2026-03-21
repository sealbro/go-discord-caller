package bot

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/caller"
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
	caller  *caller.Caller
}

// NewCommandHandlers creates a new CommandHandlers.
func NewCommandHandlers(m *manager.Service, c *caller.Caller) *CommandHandlers {
	return &CommandHandlers{manager: m, caller: c}
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

	// Owner bot channel selector
	ownerPlaceholder := "Set owner bot voice channel…"
	if chID, ok := h.manager.GetOwnerChannel(guildID); ok {
		ownerPlaceholder = fmt.Sprintf("Owner bot channel: <#%s>", chID)
	}
	components = append(components,
		discord.NewActionRow(
			discord.NewChannelSelectMenu("/owner/bind-channel", ownerPlaceholder).
				WithChannelTypes(discord.ChannelTypeGuildVoice),
		),
	)

	// Only show the "Add Speaker" button when the pool still has an unused token.
	if h.manager.HasAvailableToken(guildID) {
		components = append(components, discord.NewActionRow(
			discord.NewSuccessButton("➕ Add Speaker", "/speakers/add"),
		))
	}

	for _, sp := range speakers {
		membership, ok := sp.Guilds[guildID]
		if !ok {
			continue
		}

		label := "Enable"
		if membership.Enabled {
			label = "Disable"
		}

		placeholder := "Bind to a voice channel…"
		if membership.BoundChannelID != nil {
			placeholder = fmt.Sprintf("Bound: <#%s>", membership.BoundChannelID)
		}

		components = append(components,
			discord.NewActionRow(
				discord.NewSecondaryButton(
					fmt.Sprintf("%s %s (%s)", statusEmoji(membership.Enabled), sp.Username, label),
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

	// Also make the owner bot join its configured channel, if one is set.
	if chID, ok := h.manager.GetOwnerChannel(guildID); ok {
		if err := h.caller.JoinChannel(context.TODO(), guildID, chID); err != nil {
			// Non-fatal — log via the ephemeral warning but still report success.
			return e.CreateMessage(discord.MessageCreate{
				Content: fmt.Sprintf("🔴 **Voice raid started.** All enabled speakers have joined their bound channels.\n⚠️ Owner bot failed to join <#%s>: %s", chID, err),
			})
		}
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

	// Also make the owner bot leave its voice channel.
	_ = h.caller.LeaveChannel(context.TODO(), guildID)

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
// Discord OAuth2 invite URL pre-targeted at the current guild, and presents a
// "Confirm Registration" button to register the bot after it has been invited.
func (h *CommandHandlers) handleAddSpeakerButton(_ discord.ButtonInteractionData, e *handler.ComponentEvent) error {
	guildID, err := requireGuild(e.GuildID())
	if err != nil {
		return e.CreateMessage(ephemeral(err.Error()))
	}

	clientID, ok := h.manager.NextSpeakerClientID(guildID)
	if !ok {
		return e.CreateMessage(ephemeral("❌ All speaker tokens from the pool have already been added."))
	}

	installURL := fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=391565762894144&guild_id=%s",
		clientID, guildID,
	)

	sp, err := h.manager.AddNextSpeaker(context.TODO(), guildID)
	if err != nil {
		return e.CreateMessage(discord.MessageCreate{
			Content: "**Add Speaker Bot**\n" +
				"1. Click **Invite to Server** — the bot will be pre-selected for this server.\n" +
				"2. Complete the authorisation in the browser.\n" +
				"3. Run **/setup-speakers** again to confirm registration.",
			Components: []discord.LayoutComponent{
				discord.NewActionRow(
					discord.NewLinkButton("🔗 Invite to Server", installURL),
				),
			},
			Flags: discord.MessageFlagEphemeral,
		})
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Speaker <@%s> (`%s`) added and connected.", sp.ID, sp.Username),
		Flags:   discord.MessageFlagEphemeral,
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
		return e.CreateMessage(discord.MessageCreate{
			Content: "✅ Owner bot channel binding removed.",
			Flags:   discord.MessageFlagEphemeral,
		})
	}

	channelID := channels[0].ID
	h.manager.BindOwnerChannel(guildID, channelID)

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("✅ Owner bot will join <#%s> during voice raids.", channelID),
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
