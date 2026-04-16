package guild

import (
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
