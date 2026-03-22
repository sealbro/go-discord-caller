package domain

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/snowflake/v2"
)

// GuildMembership holds the per-guild state for a speaker bot.
type GuildMembership struct {
	Enabled bool
}

// Speaker represents a speaker bot instance that can be registered in one or more guilds.
type Speaker struct {
	ID       snowflake.ID
	BotToken string
	Username string
	Guilds   map[snowflake.ID]*GuildMembership // guildID -> per-guild state

	// Runtime state — not persisted.
	Client *bot.Client
	Cancel context.CancelFunc
}

// VoiceSession represents an active voice raid session inside a guild.
type VoiceSession struct {
	GuildID  snowflake.ID
	Active   bool
	Speakers []*Speaker // speakers currently joined to voice channels
}

// RoleBinding links a guild to the Discord role whose members' voice is captured.
type RoleBinding struct {
	GuildID snowflake.ID
	RoleID  snowflake.ID
}

// BotUserID extracts the Discord ApplicationID (= bot user ID) from a
// raw bot token.  Discord tokens are formatted as
// "<base64(userID)>.<timestamp>.<hmac>", where the first segment is the
// bot's user ID encoded with standard base64 (no padding).
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
