package domain

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/snowflake/v2"
)

// Speaker represents a speaker bot instance.
type Speaker struct {
	ID       snowflake.ID
	BotToken string
	Username string

	// Runtime state — not persisted.
	Client *bot.Client
	Cancel context.CancelFunc
}

// VoiceSession represents an active voice raid session inside a guild.
type VoiceSession struct {
	GuildID  snowflake.ID
	Active   bool
	Speakers []*Speaker
}

// BotUserID extracts the Discord ApplicationID (= bot user ID) from a raw bot token.
func BotUserID(botToken string) (snowflake.ID, bool) {
	idx := strings.IndexByte(botToken, '.')
	if idx <= 0 {
		return 0, false
	}
	data, err := base64.RawStdEncoding.DecodeString(botToken[:idx])
	if err != nil {
		return 0, false
	}
	id, err := snowflake.Parse(string(data))
	if err != nil {
		return 0, false
	}
	return id, true
}
