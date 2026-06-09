package bot

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/i18n"
)

// speakersPerPage is the maximum number of speakers shown per page in the
// speaker bind menu.  Discord allows 5 action rows per message:
//   - row 1 = toggle buttons
//   - rows 2-4 = channel-select menus (one per speaker)
//   - row 5 = navigation buttons
const speakersPerPage = 3

// localeAutoValue is the sentinel select-menu value for "no guild pin — use
// each user's interaction locale". A non-empty value is required by Discord's
// StringSelectMenuOption validation (must be 1-100 chars).
const localeAutoValue = "auto"

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

	autoOpt := discord.NewStringSelectMenuOption(loc.T("setup.locale_auto"), localeAutoValue)
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
