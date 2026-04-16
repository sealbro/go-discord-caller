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

// String returns a human-readable summary of the status.
func (s Status) String() string {
	var sb strings.Builder

	if s.GuildName != "" {
		sb.WriteString(fmt.Sprintf("\n**Guild:** %s\n", s.GuildName))
	}

	if s.CallerRoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Capture Role:** <@&%s>\n", s.CallerRoleID))
	} else {
		sb.WriteString("\n**Capture Role:** not set\n")
	}

	if s.ManagerRoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Manager Role:** <@&%s>\n", s.ManagerRoleID))
	} else {
		sb.WriteString("\n**Manager Role:** not set\n")
	}

	if chID, ok := s.BoundChannels[s.OwnerUserID]; ok {
		sb.WriteString(fmt.Sprintf("\n**Owner Bot Channel:** <#%s>\n", chID))
	} else {
		sb.WriteString("\n**Owner Bot Channel:** not set\n")
	}

	speakers := s.GetSortedSpeakers()

	sb.WriteString(fmt.Sprintf("\n**Speakers (%d):**\n", len(speakers)))
	for _, sp := range speakers {
		enabled := "🔊"
		if !sp.Enabled {
			enabled = "🔇"
		}
		bound := "unbound"
		if chID, ok := s.BoundChannels[sp.ID]; ok {
			bound = fmt.Sprintf("<#%s>", chID)
		}
		sb.WriteString(fmt.Sprintf("- %s <@%s> → %s\n", enabled, sp.ID, bound))
	}

	if s.AllyCode != "" {
		sb.WriteString(fmt.Sprintf("\n**Ally Code:** `%s`\n", s.AllyCode))
	}

	if s.Session != nil {
		if s.Session.IsGuest {
			host := s.Session.AllyCode
			if s.HostGuildName != "" {
				host = fmt.Sprintf("%s (`%s`)", s.HostGuildName, s.Session.AllyCode)
			}
			sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 guest relay → %s (%d speakers joined)\n", host, len(s.Session.Speakers)))
		} else {
			if len(s.GuestGuildNames) > 0 {
				sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined) — guests: %s\n",
					len(s.Session.Speakers), strings.Join(s.GuestGuildNames, ", ")))
			} else {
				sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
			}
		}
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}
