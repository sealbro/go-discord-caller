package domain

import "github.com/disgoorg/snowflake/v2"

// Speaker represents a speaker bot instance registered in a guild.
type Speaker struct {
	ID              snowflake.ID
	GuildID         snowflake.ID
	BotToken        string
	Username        string
	AllowedChannels []snowflake.ID // voice channels this speaker is permitted to join
	BoundChannelID  *snowflake.ID  // currently bound voice channel (nil = unbound)
	Enabled         bool
}

// HasChannelAccess reports whether the speaker is allowed to join the given channel.
func (s *Speaker) HasChannelAccess(channelID snowflake.ID) bool {
	for _, id := range s.AllowedChannels {
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
