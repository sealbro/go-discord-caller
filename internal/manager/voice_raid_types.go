package manager

import (
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/pool"
)

// ChannelAccessWarning describes a bot that cannot connect or speak in its bound channel.
type ChannelAccessWarning struct {
	BotID     snowflake.ID
	ChannelID snowflake.ID
}

// reconnectBackoff is the pause between a reconnect's two join attempts.
const reconnectBackoff = 2 * time.Second

// reconnectBudget bounds one whole reconnect: both join attempts and the
// backoff between them. A failed attempt costs the bounded handshake *and* the
// cleanup Leave that follows it, so sizing this off the handshake alone leaves
// the budget already expired when the retry is due.
const reconnectBudget = 2*pool.VoiceJoinCost + reconnectBackoff

// voiceLeaveTimeout is the maximum time to wait for a voice Leave call.
// Using context.Background() without a deadline risks hanging forever if Discord
// is unresponsive during session teardown.
const voiceLeaveTimeout = 5 * time.Second
