package manager

import (
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// ChannelAccessWarning describes a bot that cannot connect or speak in its bound channel.
type ChannelAccessWarning struct {
	BotID     snowflake.ID
	ChannelID snowflake.ID
}

// voiceLeaveTimeout is the maximum time to wait for a voice Leave call.
// Using context.Background() without a deadline risks hanging forever if Discord
// is unresponsive during session teardown.
const voiceLeaveTimeout = 5 * time.Second
