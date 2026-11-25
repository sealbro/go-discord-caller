package ally

import (
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// Relay is what a raid's audio pipeline needs from the inter-guild session it
// belongs to. Implemented by *Session.
type Relay interface {
	// HostRaidMode is the raid mode the hosting guild started the session in.
	HostRaidMode() guild.RaidMode
	BroadcastFromGuild(srcGuildID snowflake.ID, pkt []byte)
	AddListener(guildID snowflake.ID, chs []chan<- []byte)
	AddReceiver(guildID snowflake.ID, sink RelaySink)
	Attached(guildID snowflake.ID) bool
	SetCapturing(guildID snowflake.ID)
	HasCapturingPeers(guildID snowflake.ID) bool
	SetRouteObserver(guildID snowflake.ID, fn func())
	RemoveGuild(guildID snowflake.ID)
	GuestGuildIDs() []snowflake.ID
}

var _ Relay = (*Session)(nil)

// HostRaidMode returns s.HostMode.
func (s *Session) HostRaidMode() guild.RaidMode { return s.HostMode }
