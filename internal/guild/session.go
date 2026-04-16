package guild

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
)

// Session represents an active voice raid session inside a guild.
// When returned as part of a guild.Status snapshot (via GetStatus), Cancel and
// Cleanup are always nil — snapshots are read-only display objects.
type Session struct {
	GuildID  snowflake.ID
	Speakers []Speaker
	Cancel   context.CancelFunc
	Cleanup  func() // closes providers/receivers; safe to call multiple times (uses sync.Once internally)
	AllyCode string // relay code for this session (always set on host; set to joined code on guest)
	IsGuest  bool   // true when this guild joined another guild's session
}
