package pool

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// GuildVoice manages join/leave for one bot's voice connection in a guild.
// Obtain via pool.Service.VoiceFor or manager.Service.ownerVoice.
type GuildVoice struct {
	vm        voice.Manager
	channelID snowflake.ID // zero when unbound
}

// NewGuildVoice creates a GuildVoice for the given voice manager and channel.
func NewGuildVoice(vm voice.Manager, channelID snowflake.ID) GuildVoice {
	return GuildVoice{vm: vm, channelID: channelID}
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

// Leave closes the bot's current voice connection in the guild, if any.
func (v GuildVoice) Leave(ctx context.Context, guildID snowflake.ID) {
	if conn := v.vm.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}
}
