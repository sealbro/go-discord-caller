package guild

import (
	"fmt"
	"slices"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// Status is the per-guild state managed exclusively by manager.Service.
// All mutations go through the manager; callers receive a value copy from GetStatus.
type Status struct {
	GuildID         snowflake.ID
	GuildName       string                        // display name of this guild
	OwnerUserID     snowflake.ID                  // owner bot user ID; look up channel via BoundChannels[OwnerUserID]
	Speakers        map[snowflake.ID]*Speaker     // speakerID -> speaker (Enabled carries per-guild state)
	BoundChannels   map[snowflake.ID]snowflake.ID // userID -> channelID
	CallerRoleID    *snowflake.ID                 // caller role: members whose voice is captured
	ManagerRoleID   *snowflake.ID                 // manager role: members who can setup/start/stop the bot
	AllyCode        string                        // persistent relay code for this guild (always set after seeding)
	Session         *Session                      // nil when no active session; in snapshots Cancel and Cleanup are always nil
	HostGuildName   string                        // set when this guild is a guest in another guild's relay session
	GuestGuildNames []string                      // set when this guild is the host: names of connected guest guilds
}

func NewStatus(guildID snowflake.ID, ownerUserID snowflake.ID) *Status {
	return &Status{
		GuildID:       guildID,
		OwnerUserID:   ownerUserID,
		Speakers:      make(map[snowflake.ID]*Speaker, 2),
		BoundChannels: make(map[snowflake.ID]snowflake.ID, 2),
	}
}

// HasActiveSession reports whether there is a running voice raid.
func (s Status) HasActiveSession() bool {
	return s.Session != nil
}

func (s Status) GetSortedSpeakers() []*Speaker {
	return slices.SortedFunc(func(yield func(*Speaker) bool) {
		for _, sp := range s.Speakers {
			if !yield(sp) {
				return
			}
		}
	}, func(a, b *Speaker) int { return strings.Compare(a.Username, b.Username) })
}

// Render returns a human-readable, localized summary of the status. Pass a
// localizer obtained from internal/i18n. A nil loc uses English fallbacks
// (useful for logs and tests).
func (s Status) Render(loc Translator) string {
	t := func(key string, args ...any) string {
		if loc == nil {
			return englishStatusFallback(key, args...)
		}
		return loc.T(key, args...)
	}

	var sb strings.Builder

	if s.GuildName != "" {
		fmt.Fprintf(&sb, "\n**%s:** %s\n", t("status.guild"), s.GuildName)
	}

	if s.CallerRoleID != nil {
		fmt.Fprintf(&sb, "\n**%s:** <@&%s>\n", t("status.capture_role"), s.CallerRoleID)
	} else {
		fmt.Fprintf(&sb, "\n**%s:** %s\n", t("status.capture_role"), t("status.not_set"))
	}

	if s.ManagerRoleID != nil {
		fmt.Fprintf(&sb, "\n**%s:** <@&%s>\n", t("status.manager_role"), s.ManagerRoleID)
	} else {
		fmt.Fprintf(&sb, "\n**%s:** %s\n", t("status.manager_role"), t("status.not_set"))
	}

	if chID, ok := s.BoundChannels[s.OwnerUserID]; ok {
		fmt.Fprintf(&sb, "\n**%s:** <#%s>\n", t("status.owner_channel"), chID)
	} else {
		fmt.Fprintf(&sb, "\n**%s:** %s\n", t("status.owner_channel"), t("status.not_set"))
	}

	speakers := s.GetSortedSpeakers()

	fmt.Fprintf(&sb, "\n**%s**\n", t("status.speakers_header", "Count", len(speakers)))
	unbound := t("status.unbound")
	for _, sp := range speakers {
		enabled := "🔊"
		if !sp.Enabled {
			enabled = "🔇"
		}
		bound := unbound
		if chID, ok := s.BoundChannels[sp.ID]; ok {
			bound = fmt.Sprintf("<#%s>", chID)
		}
		fmt.Fprintf(&sb, "- %s <@%s> → %s\n", enabled, sp.ID, bound)
	}

	if s.AllyCode != "" {
		fmt.Fprintf(&sb, "\n**%s:** `%s`\n", t("status.ally_code"), s.AllyCode)
	}

	raidLabel := t("status.raid")
	if s.Session != nil {
		if s.Session.IsGuest {
			host := s.Session.AllyCode
			if s.HostGuildName != "" {
				host = fmt.Sprintf("%s (`%s`)", s.HostGuildName, s.Session.AllyCode)
			}
			fmt.Fprintf(&sb, "\n**%s:** %s\n", raidLabel,
				t("status.raid_guest", "Count", len(s.Session.Speakers), "Host", host))
		} else if len(s.GuestGuildNames) > 0 {
			fmt.Fprintf(&sb, "\n**%s:** %s\n", raidLabel,
				t("status.raid_active_with_guests", "Count", len(s.Session.Speakers), "Guests", strings.Join(s.GuestGuildNames, ", ")))
		} else {
			fmt.Fprintf(&sb, "\n**%s:** %s\n", raidLabel,
				t("status.raid_active", "Count", len(s.Session.Speakers)))
		}
	} else {
		fmt.Fprintf(&sb, "\n**%s:** %s\n", raidLabel, t("status.raid_inactive"))
	}

	return sb.String()
}

// englishStatusFallback renders status labels using en.yaml-equivalent strings
// when no localizer is supplied. Keep keys in sync with en.yaml.
func englishStatusFallback(key string, args ...any) string {
	count := -1
	host := ""
	guests := ""
	for i := 0; i+1 < len(args); i += 2 {
		name, _ := args[i].(string)
		switch name {
		case "Count":
			if n, ok := args[i+1].(int); ok {
				count = n
			}
		case "Host":
			host, _ = args[i+1].(string)
		case "Guests":
			guests, _ = args[i+1].(string)
		}
	}
	switch key {
	case "status.guild":
		return "Guild"
	case "status.capture_role":
		return "Capture Role"
	case "status.manager_role":
		return "Manager Role"
	case "status.owner_channel":
		return "Owner Bot Channel"
	case "status.not_set":
		return "not set"
	case "status.unbound":
		return "unbound"
	case "status.ally_code":
		return "Ally Code"
	case "status.raid":
		return "Voice Raid"
	case "status.raid_inactive":
		return "⚫ inactive"
	case "status.speakers_header":
		return fmt.Sprintf("Speakers (%d):", count)
	case "status.raid_active":
		return fmt.Sprintf("🔴 active (%d speakers joined)", count)
	case "status.raid_active_with_guests":
		return fmt.Sprintf("🔴 active (%d speakers joined) — guests: %s", count, guests)
	case "status.raid_guest":
		return fmt.Sprintf("🔴 guest relay → %s (%d speakers joined)", host, count)
	}
	return key
}
