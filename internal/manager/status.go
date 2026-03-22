package manager

import (
	"fmt"
	"strings"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// Status holds the current state of speakers and bindings in a guild.
type Status struct {
	GuildID        snowflake.ID
	Speakers       []*domain.Speaker
	BoundChannels  map[snowflake.ID]snowflake.ID // userID -> channelID
	RoleID         *snowflake.ID
	OwnerChannelID *snowflake.ID
	Session        *domain.VoiceSession
}

// String returns a human-readable summary of the status.
func (s *Status) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
		membership, ok := sp.Guilds[s.GuildID]
		if !ok {
			continue
		}
		enabled := "✅"
		if !membership.Enabled {
			enabled = "❌"
		}
		bound := "unbound"
		if chID, ok := s.BoundChannels[sp.ID]; ok {
			bound = fmt.Sprintf("<#%s>", chID)
		}
		sb.WriteString(fmt.Sprintf("- %s <@%s> → %s\n", enabled, sp.ID, bound))
	}

	if s.RoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Capture Role:** <@&%s>\n", s.RoleID))
	} else {
		sb.WriteString("\n**Capture Role:** not set\n")
	}

	if s.OwnerChannelID != nil {
		sb.WriteString(fmt.Sprintf("\n**Owner Bot Channel:** <#%s>\n", s.OwnerChannelID))
	} else {
		sb.WriteString("\n**Owner Bot Channel:** not set\n")
	}

	if s.Session != nil && s.Session.Active {
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}
