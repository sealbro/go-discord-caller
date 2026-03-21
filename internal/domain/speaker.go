package domain

import "github.com/disgoorg/snowflake/v2"

// GuildMembership holds the per-guild state for a speaker bot.
type GuildMembership struct {
	AllowedChannels []snowflake.ID // voice channels this speaker is permitted to join in this guild
	Enabled         bool
}

// Speaker represents a speaker bot instance that can be registered in one or more guilds.
type Speaker struct {
	ID       snowflake.ID
	BotToken string
	Username string
	Guilds   map[snowflake.ID]*GuildMembership // guildID -> per-guild state
}

// HasChannelAccess reports whether the speaker is allowed to join the given channel in the guild.
// If the guild's AllowedChannels is empty, access is unrestricted (all voice channels allowed).
// Returns false if the speaker is not registered in the guild.
func (s *Speaker) HasChannelAccess(guildID, channelID snowflake.ID) bool {
	m, ok := s.Guilds[guildID]
	if !ok {
		return false
	}
	if len(m.AllowedChannels) == 0 {
		return true // no restriction — all channels are allowed
	}
	for _, id := range m.AllowedChannels {
		if id == channelID {
			return true
		}
	}
	return false
}

// VoiceSession represents an active voice raid session inside a guild.
type VoiceSession struct {
	GuildID    snowflake.ID
	Active     bool
	SpeakerIDs []snowflake.ID // speakers currently joined to voice channels
}

// RoleBinding links a guild to the Discord role whose members' voice is captured.
type RoleBinding struct {
	GuildID snowflake.ID
	RoleID  snowflake.ID
}
