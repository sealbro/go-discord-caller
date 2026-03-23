package domain

import (
	"fmt"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// GuildStatus is the per-guild state managed exclusively by manager.Service.
// All mutations go through the manager; callers receive a value copy from GetStatus.
type GuildStatus struct {
	GuildID       snowflake.ID
	OwnerUserID   snowflake.ID                  // owner bot user ID; look up channel via BoundChannels[OwnerUserID]
	Speakers      map[snowflake.ID]*Speaker     // speakerID -> speaker (Enabled carries per-guild state)
	BoundChannels map[snowflake.ID]snowflake.ID // userID -> channelID
	RoleID        *snowflake.ID
	Session       *VoiceSession // nil when no active session
}

func NewGuildStatus(guildID snowflake.ID, ownerUserID snowflake.ID) *GuildStatus {
	return &GuildStatus{
		GuildID:       guildID,
		OwnerUserID:   ownerUserID,
		Speakers:      make(map[snowflake.ID]*Speaker, 2),
		BoundChannels: make(map[snowflake.ID]snowflake.ID, 2),
	}
}

// HasActiveSession reports whether there is a running voice raid.
func (s GuildStatus) HasActiveSession() bool {
	return s.Session != nil
}

// String returns a human-readable summary of the status.
func (s GuildStatus) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
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

	if s.RoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Capture Role:** <@&%s>\n", s.RoleID))
	} else {
		sb.WriteString("\n**Capture Role:** not set\n")
	}

	if chID, ok := s.BoundChannels[s.OwnerUserID]; ok {
		sb.WriteString(fmt.Sprintf("\n**Owner Bot Channel:** <#%s>\n", chID))
	} else {
		sb.WriteString("\n**Owner Bot Channel:** not set\n")
	}

	if s.Session != nil {
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}
