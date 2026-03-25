package domain

import (
	"fmt"
	"slices"
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
	CallerRoleID  *snowflake.ID                 // caller role: members whose voice is captured
	ManagerRoleID *snowflake.ID                 // manager role: members who can setup/start/stop the bot
	Session       *VoiceSession                 // nil when no active session
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

func (s GuildStatus) GetSortedSpeakers() []*Speaker {
	return slices.SortedFunc(func(yield func(*Speaker) bool) {
		for _, sp := range s.Speakers {
			if !yield(sp) {
				return
			}
		}
	}, func(a, b *Speaker) int { return strings.Compare(a.Username, b.Username) })
}

// String returns a human-readable summary of the status.
func (s GuildStatus) String() string {
	var sb strings.Builder

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

	if s.Session != nil {
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}
