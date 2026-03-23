package domain

import (
	"fmt"
	"strings"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// GuildStatus Status is the view model returned by GetStatus — built from StatusStore + SessionStore.
type GuildStatus struct {
	mu             sync.RWMutex
	GuildID        snowflake.ID
	Speakers       []*Speaker                    // all speakers registered for this guild
	Enabled        map[snowflake.ID]bool         // speakerID -> enabled
	BoundChannels  map[snowflake.ID]snowflake.ID // userID -> channelID
	RoleID         *snowflake.ID
	OwnerChannelID *snowflake.ID
	Session        *VoiceSession
}

func NewGuildStatus(guildID snowflake.ID) *GuildStatus {
	return &GuildStatus{
		GuildID:        guildID,
		Speakers:       make([]*Speaker, 0),
		Enabled:        make(map[snowflake.ID]bool, 2),
		BoundChannels:  make(map[snowflake.ID]snowflake.ID, 2),
		RoleID:         nil,
		OwnerChannelID: nil,
		Session:        nil,
	}
}

func (s *GuildStatus) HasActiveSession() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Session != nil
}

func (s *GuildStatus) SetSession(session *VoiceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Session = session
}

// String returns a human-readable summary of the status.
func (s *GuildStatus) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
		enabled := "✅"
		if !s.Enabled[sp.ID] {
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

	if s.HasActiveSession() {
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}
