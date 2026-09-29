package pool

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// disgo's Open and Close both block on nothing but the caller's context, and
// the raid context they are given has no deadline.
const (
	VoiceJoinTimeout    = 15 * time.Second
	voiceCleanupTimeout = 5 * time.Second
)

// GuildVoice manages join/leave for one bot's voice connection in a guild.
// Obtain via pool.Service.VoiceFor or manager.Service.ownerVoice.
type GuildVoice struct {
	vm        voice.Manager
	channelID snowflake.ID // zero when unbound

	// zero means the package default
	joinTimeout    time.Duration
	cleanupTimeout time.Duration
}

func (v GuildVoice) openTimeout() time.Duration {
	if v.joinTimeout > 0 {
		return v.joinTimeout
	}
	return VoiceJoinTimeout
}

func (v GuildVoice) leaveTimeout() time.Duration {
	if v.cleanupTimeout > 0 {
		return v.cleanupTimeout
	}
	return voiceCleanupTimeout
}

// NewGuildVoice creates a GuildVoice for the given voice manager and channel.
func NewGuildVoice(vm voice.Manager, channelID snowflake.ID) GuildVoice {
	return GuildVoice{vm: vm, channelID: channelID}
}

// ChannelID returns the bound channel ID (zero when unbound).
func (v GuildVoice) ChannelID() snowflake.ID { return v.channelID }

// Join connects to the bound voice channel and returns the connection.
// Returns nil conn (and nil error) when no channel is bound.
// The handshake is bounded by VoiceJoinTimeout regardless of ctx.
func (v GuildVoice) Join(ctx context.Context, guildID snowflake.ID) (voice.Conn, error) {
	if v.channelID == 0 {
		return nil, nil
	}
	openCtx, cancel := context.WithTimeout(ctx, v.openTimeout())
	defer cancel()
	conn := v.vm.CreateConn(guildID)
	if err := conn.Open(openCtx, v.channelID, false, false); err != nil {
		// A half-open conn stays registered, and CreateConn would hand the
		// same one to the next session, where Open returns instantly off its
		// stale token without being connected. Detached from ctx because a
		// cancelled one drops the leave op inside disgo's rate limiter,
		// leaving the bot in the channel.
		leaveCtx, leaveCancel := context.WithTimeout(context.WithoutCancel(ctx), v.leaveTimeout())
		v.Leave(leaveCtx, guildID)
		leaveCancel()
		return nil, fmt.Errorf("join channel %s: %w", v.channelID, err)
	}
	return conn, nil
}

// Leave closes the bot's current voice connection in the guild, if any.
// Safe on a connection whose Open never completed — see safeUDPConn, which the
// voice manager installs via SafeUDPConnOpt.
//
// disgo's connImpl.Close does not stop the audio sender, and the sender's loop
// does not exit on a provider error either — so once teardown closes the
// provider, a surviving sender logs an ERROR every 20 ms forever. Closing the
// registered sender afterwards is what actually stops that goroutine; see
// AudioSenderRegistry.
func (v GuildVoice) Leave(ctx context.Context, guildID snowflake.ID) {
	if conn := v.vm.GetConn(guildID); conn != nil {
		conn.Close(ctx)
		CloseAudioSender(conn)
	}
}
