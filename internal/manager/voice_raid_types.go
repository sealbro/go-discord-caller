package manager

import (
	"context"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// reconnectApplier re-applies voice provider/receiver to a freshly opened conn
// after a bot reconnects to its bound channel mid-session. It creates new
// provider/receiver objects from the same channels so the mixer graph stays connected.
// ctx is the reconnect context (not the original session context) so that metric
// recording and speaking-flag ops use a live, uncancelled context.
type reconnectApplier func(ctx context.Context, conn voice.Conn)

// ChannelAccessWarning describes a bot that cannot connect or speak in its bound channel.
type ChannelAccessWarning struct {
	BotID     snowflake.ID
	ChannelID snowflake.ID
}

// voiceLeaveTimeout is the maximum time to wait for a voice Leave call.
// Using context.Background() without a deadline risks hanging forever if Discord
// is unresponsive during session teardown.
const voiceLeaveTimeout = 5 * time.Second

// relayInputID is the synthetic source ID used when adding a guest relay feed
// as an input to a host-side ChannelMixer. Discord snowflakes are epoch-based
// (minimum value ~4 billion) so 1 never collides with a real user/bot ID.
const relayInputID snowflake.ID = 1

// sourceEntry is one audio capture channel feeding the relay mixer graph.
type sourceEntry struct {
	id        snowflake.ID
	channelID snowflake.ID
	ch        <-chan []byte
}

// destChannel groups all speaker output channels that share the same voice channel.
type destChannel struct {
	channelID snowflake.ID
	outs      []chan<- []byte
}

// speakerResult holds the outcome of a single successfully joined speaker.
type speakerResult struct {
	speaker   guild.Speaker
	chOut     chan<- []byte
	chCapture <-chan []byte // nil when withCapture is false
	gv        pool.GuildVoice
	cleanup   func() // closes provider/receiver; caller must invoke on teardown
}

// raidSetup captures the common setup result for both host and guest flows.
type raidSetup struct {
	joined         []speakerResult
	speakers       []guild.Speaker
	speakerCleanup func()
	outs           []chan<- []byte
}
