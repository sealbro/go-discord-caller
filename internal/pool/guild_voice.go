package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/trace"
)

// disgo's Open and Close both block on nothing but the caller's context, and
// the raid context they are given has no deadline.
//
// UPSTREAM(disgo v0.20.0): Open and Close bound nothing themselves.
//
// A handshake that hangs is asked once more. Successful joins took at most 2.5 s
// in production, and the hangs of 2026-11-19 were Discord accepting the bot
// into the channel without ever assigning it a voice server, which a fresh
// join request usually got past. The split keeps a failed Join as long as the
// single 15 s attempt it replaces.
const (
	voiceJoinFirstTimeout  = 6 * time.Second
	voiceRetryLeaveTimeout = 2 * time.Second
	voiceJoinRetryTimeout  = 7 * time.Second
	voiceCleanupTimeout    = 5 * time.Second
)

// VoiceJoinCost is the longest a single failed Join can take: both bounded
// handshakes, the leave between them, and the cleanup Leave that follows,
// which waits on disgo's Close for a conn that never finished opening.
// Callers budgeting more than one Join must use this.
const VoiceJoinCost = voiceJoinFirstTimeout + voiceRetryLeaveTimeout + voiceJoinRetryTimeout + voiceCleanupTimeout

// GuildVoice manages join/leave for one bot's voice connection in a guild.
// Obtain via pool.Service.VoiceFor or manager.Service.ownerVoice.
type GuildVoice struct {
	vm        voice.Manager
	channelID snowflake.ID // zero when unbound

	// botID is the bot this connection belongs to; zero when the caller did
	// not say, which only costs the region bookkeeping in Leave.
	botID snowflake.ID
}

// ForBot records which bot this GuildVoice acts for, so Leave can retire the
// bot's voice-region series itself rather than trusting the GuildVoiceLeave
// event to arrive.
func (v GuildVoice) ForBot(botID snowflake.ID) GuildVoice {
	v.botID = botID
	return v
}

// NewGuildVoice creates a GuildVoice for the given voice manager and channel.
func NewGuildVoice(vm voice.Manager, channelID snowflake.ID) GuildVoice {
	return GuildVoice{vm: vm, channelID: channelID}
}

// ChannelID returns the bound channel ID (zero when unbound).
func (v GuildVoice) ChannelID() snowflake.ID { return v.channelID }

// Join connects to the bound voice channel and returns the connection.
// Returns nil conn (and nil error) when no channel is bound.
// A handshake that hangs is retried once; the whole of it is bounded by
// VoiceJoinCost regardless of ctx.
func (v GuildVoice) Join(ctx context.Context, guildID snowflake.ID) (voice.Conn, error) {
	if v.channelID == 0 {
		return nil, nil
	}
	defer defaultJoinTraces.begin(v.botID, guildID, trace.SpanFromContext(ctx))()

	conn, err := v.open(ctx, guildID, voiceJoinFirstTimeout)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		trace.SpanFromContext(ctx).AddEvent("voice.join.retry")
		v.release(ctx, guildID, voiceRetryLeaveTimeout)
		if conn, err = v.open(ctx, guildID, voiceJoinRetryTimeout); err == nil {
			return conn, nil
		}
	}
	v.release(ctx, guildID, voiceCleanupTimeout)
	return nil, fmt.Errorf("join channel %s: %w", v.channelID, err)
}

func (v GuildVoice) open(ctx context.Context, guildID snowflake.ID, timeout time.Duration) (voice.Conn, error) {
	openCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := v.vm.CreateConn(guildID)
	if err := conn.Open(openCtx, v.channelID, false, false); err != nil {
		return nil, err
	}
	return conn, nil
}

// release leaves after a failed handshake. A half-open conn stays registered,
// and CreateConn would hand the same one to the next attempt, where Open
// returns instantly off its stale token without being connected. Detached from
// ctx because a cancelled one drops the leave op inside disgo's rate limiter,
// leaving the bot in the channel.
func (v GuildVoice) release(ctx context.Context, guildID snowflake.ID, timeout time.Duration) {
	leaveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	v.Leave(leaveCtx, guildID)
}

// Leave closes the bot's current voice connection in the guild, if any. The
// conn's audio sender, which disgo's connImpl.Close leaves running, is stopped
// when the conn is removed; see newConn.
func (v GuildVoice) Leave(ctx context.Context, guildID snowflake.ID) {
	if conn := v.vm.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}
	ForgetVoiceRegion(v.botID, guildID)
}
