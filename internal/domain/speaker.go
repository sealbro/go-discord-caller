package domain

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// Speaker represents a speaker bot instance.
type Speaker struct {
	ID       snowflake.ID
	Username string
	Enabled  bool
}

// VoiceSession represents an active voice raid session inside a guild.
type VoiceSession struct {
	GuildID   snowflake.ID
	Speakers  []Speaker
	Cancel    context.CancelFunc
	Cleanup   func() // closes providers/receivers; safe to call multiple times (uses sync.Once internally)
	RelayCode string // set for host sessions; empty for standalone raids
	IsGuest   bool   // true when this guild joined another guild's session
}

// BotUserID extracts the Discord ApplicationID (= bot user ID) from a raw bot token.
func BotUserID(botToken string) (snowflake.ID, bool) {
	idx := strings.IndexByte(botToken, '.')
	if idx <= 0 {
		return 0, false
	}
	data, err := base64.RawURLEncoding.DecodeString(botToken[:idx])
	if err != nil {
		return 0, false
	}
	id, err := snowflake.Parse(string(data))
	if err != nil {
		return 0, false
	}
	return id, true
}
