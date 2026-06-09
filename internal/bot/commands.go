package bot

import (
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// BuildCommands returns the slash command list with localized descriptions
// (and option choice names) attached from the i18n bundle. Command names stay
// in English; only descriptions and option choices are localized.
//
// Top-level Description fields render in English (Discord shows these to users
// whose client locale is not present in DescriptionLocalizations). The
// per-locale Localizations map covers every other supported locale.
func BuildCommands(bundle *i18n.Bundle) []discord.ApplicationCommandCreate {
	def := bundle.For("", "")

	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:                     "setup",
			Description:              def.T("cmd.setup.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.setup.description"),
		},
		discord.SlashCommandCreate{
			Name:                     "start",
			Description:              def.T("cmd.start.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.description"),
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:                     "code",
					Description:              def.T("cmd.start.opt.code.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.opt.code.description"),
					Required:                 false,
				},
				discord.ApplicationCommandOptionString{
					Name:                     "mode",
					Description:              def.T("cmd.start.opt.mode.description"),
					DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.start.opt.mode.description"),
					Required:                 false,
					Choices: []discord.ApplicationCommandOptionChoiceString{
						{Name: def.T("cmd.start.opt.mode.choice.one"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.one"), Value: callerModeOne},
						{Name: def.T("cmd.start.opt.mode.choice.many"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.many"), Value: callerModeMany},
						{Name: def.T("cmd.start.opt.mode.choice.one_many"), NameLocalizations: bundle.NameLocalizations("cmd.start.opt.mode.choice.one_many"), Value: callerModeOneMany},
					},
				},
			},
		},
		discord.SlashCommandCreate{
			Name:                     "stop",
			Description:              def.T("cmd.stop.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.stop.description"),
		},
		discord.SlashCommandCreate{
			Name:                     "status",
			Description:              def.T("cmd.status.description"),
			DescriptionLocalizations: bundle.DescriptionLocalizations("cmd.status.description"),
		},
	}
}

// callerModeChoice is the value of the "mode" slash command option.
// It maps to different RaidModes depending on whether a relay code is supplied:
//   - no code (host): callerModeOne → RaidModeOneCaller, callerModeMany → RaidModeGuildCaller, callerModeOneMany → RaidModeOneManyGuildCaller
//   - with code (guest): callerModeOne → RaidModeAllyListener, callerModeMany → RaidModeAllyCaller, callerModeOneMany → RaidModeOneManyAllyCaller
const (
	callerModeOne     string = "one"
	callerModeMany    string = "many"
	callerModeOneMany string = "one_many"
)

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
