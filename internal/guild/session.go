package guild

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
)

// AllowUpdater allows event handlers to push per-user allow decisions into an
// active session's filter without importing the manager package.
// Implemented by *manager.AllowFilter.
type AllowUpdater interface {
	Set(userID snowflake.ID, allowed bool)
}

// MixerPauser is the subset of opus.Mixer used for pause/resume.
// Defined here to avoid a dependency from guild → opus.
type MixerPauser interface {
	SetPaused(bool)
}

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

	// ChannelMixers maps voice channel ID → mixer for that channel.
	// Used to pause/resume mixers when users join/leave channels.
	// Nil in snapshots.
	ChannelMixers map[snowflake.ID]MixerPauser

	// AllowFilter receives per-user allow-decision updates from event handlers.
	// Nil in snapshots.
	AllowFilter AllowUpdater
}
