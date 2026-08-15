package pool

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// SessionCloser releases the DAVE session bound to a voice connection that is
// being discarded. *dave.Registry implements it; a nil SessionCloser is fine
// and means nothing needs releasing (the libdave backend, or tests).
//
// This exists because disgo never closes the DAVE session itself, and the
// dave-go backend runs watchdog goroutines that only stop on Close — see
// dave.Registry for the full story. The key is the voice.Conn: it is the same
// value disgo handed the session factory as godave.Callbacks.
type SessionCloser interface {
	Release(key any)
}

// GuildVoice manages join/leave for one bot's voice connection in a guild.
// Obtain via pool.Service.VoiceFor or manager.Service.ownerVoice.
type GuildVoice struct {
	vm        voice.Manager
	channelID snowflake.ID // zero when unbound
	sessions  SessionCloser
}

// NewGuildVoice creates a GuildVoice for the given voice manager and channel.
// sessions may be nil.
func NewGuildVoice(vm voice.Manager, channelID snowflake.ID, sessions SessionCloser) GuildVoice {
	return GuildVoice{vm: vm, channelID: channelID, sessions: sessions}
}

// ChannelID returns the bound channel ID (zero when unbound).
func (v GuildVoice) ChannelID() snowflake.ID { return v.channelID }

// Join connects to the bound voice channel and returns the connection.
// Returns nil conn (and nil error) when no channel is bound.
func (v GuildVoice) Join(ctx context.Context, guildID snowflake.ID) (voice.Conn, error) {
	if v.channelID == 0 {
		return nil, nil
	}
	conn := v.vm.CreateConn(guildID)
	if err := conn.Open(ctx, v.channelID, false, false); err != nil {
		return nil, fmt.Errorf("join channel %s: %w", v.channelID, err)
	}
	return conn, nil
}

// Leave closes the bot's current voice connection in the guild, if any, and
// releases the DAVE session that was bound to it.
func (v GuildVoice) Leave(ctx context.Context, guildID snowflake.ID) {
	conn := v.vm.GetConn(guildID)
	if conn == nil {
		return
	}
	conn.Close(ctx)
	// After Close, so the session stays alive for whatever the teardown still
	// sends over the connection.
	if v.sessions != nil {
		v.sessions.Release(conn)
	}
}
