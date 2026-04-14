package guild

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
)

// Session represents an active voice raid session inside a guild.
type Session struct {
	GuildID  snowflake.ID
	Speakers []Speaker
	Cancel   context.CancelFunc
	Cleanup  func() // closes providers/receivers; safe to call multiple times (uses sync.Once internally)
	AllyCode string // set for host sessions; empty for standalone raids
	IsGuest  bool   // true when this guild joined another guild's session
}
