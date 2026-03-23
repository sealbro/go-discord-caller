package domain

import "github.com/disgoorg/snowflake/v2"

// GuildStatus holds the per-guild speaker registration state persisted in the StatusStore.
// Channel and role bindings are owned by store.Store and looked up separately.
type GuildStatus struct {
	GuildID  snowflake.ID
	Speakers map[snowflake.ID]bool // speakerID -> enabled
}

// NewGuildStatus creates an empty GuildStatus for the given guild.
func NewGuildStatus(guildID snowflake.ID) *GuildStatus {
	return &GuildStatus{
		GuildID:  guildID,
		Speakers: make(map[snowflake.ID]bool),
	}
}
