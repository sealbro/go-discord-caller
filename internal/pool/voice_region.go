package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// VoiceRegionListeners reports the voice region botID's connections land on.
// Every client this process builds should register them, because the region is
// a property of the voice channel — the owner bot and a speaker bot in the same
// guild can sit in channels with different rtc_region overrides.
//
// VOICE_SERVER_UPDATE is the only source: a channel left on automatic carries
// no rtc_region to read, and disgo's voice.Conn does not expose the endpoint it
// connected to.
func VoiceRegionListeners(botID snowflake.ID, metrics *telemetry.VoiceRegionMetrics) []bot.EventListener {
	regionMetrics.Store(botID, metrics)

	return []bot.EventListener{
		bot.NewListenerFunc(func(e *events.VoiceServerUpdate) {
			if e.Endpoint == nil {
				return
			}
			metrics.SetRegion(e.GuildID, botID, *e.Endpoint)
		}),
		bot.NewListenerFunc(func(e *events.GuildVoiceLeave) {
			if e.VoiceState.UserID != botID {
				return
			}
			metrics.ForgetRegion(e.VoiceState.GuildID, botID)
		}),
	}
}

// regionMetrics maps a bot to the metrics its listeners report into, so
// teardown can retire a series without the caller having to carry the
// collector around. Registered when the bot's listeners are built.
var regionMetrics sync.Map // snowflake.ID -> *telemetry.VoiceRegionMetrics

// ForgetVoiceRegion retires botID's region series for guildID. Called by
// GuildVoice.Leave, because the GuildVoiceLeave event above is the same kind of
// event this package already treats as unreliable: when a gateway dies
// mid-teardown it never arrives, and the region is live state that would then
// be reported for the rest of the process's life. Both paths are needed — the
// event also covers a bot Discord moved out of voice without us asking.
func ForgetVoiceRegion(botID, guildID snowflake.ID) {
	if botID == 0 {
		return
	}
	if m, ok := regionMetrics.Load(botID); ok {
		m.(*telemetry.VoiceRegionMetrics).ForgetRegion(guildID, botID)
	}
}

// staleAfterHeartbeats is how many heartbeat intervals without an ACK it takes
// for the last round trip to stop being reported.
const staleAfterHeartbeats = 2

// ackLatency holds the last heartbeat round trip measured on a voice conn.
// disgo's Gateway.Latency is no substitute: it is read unsynchronised against
// the heartbeat goroutine, and until the first ACK it reports the handshake
// time as if it were a round trip.
type ackLatency struct {
	rtt      atomic.Int64 // nanoseconds
	ackedAt  atomic.Int64 // Unix nanoseconds
	interval atomic.Int64 // nanoseconds, from the gateway's Hello
}

// observe learns the heartbeat interval from Hello and times each ACK against
// the send time its nonce carries, which disgo sets to the send time in Unix
// milliseconds.
func (a *ackLatency) observe(op voice.Opcode, data voice.GatewayMessageData) {
	switch d := data.(type) {
	case voice.GatewayMessageDataHello:
		if op == voice.OpcodeHello {
			a.interval.Store(int64(d.HeartbeatInterval * float64(time.Millisecond)))
		}
	case voice.GatewayMessageDataHeartbeatACK:
		if op == voice.OpcodeHeartbeatACK {
			now := time.Now()
			a.rtt.Store(int64(now.Sub(time.UnixMilli(d.T))))
			a.ackedAt.Store(now.UnixNano())
		}
	}
}

func (a *ackLatency) read() time.Duration {
	interval := time.Duration(a.interval.Load())
	if interval > 0 && time.Since(time.Unix(0, a.ackedAt.Load())) > staleAfterHeartbeats*interval {
		return 0
	}
	return time.Duration(a.rtt.Load())
}

// watchVoiceLatency reports latency for as long as the conn exists, and returns
// the function that stops it. It is tied to the conn rather than to the region
// events: a reopened conn keeps its gateway, and the region is forgotten and
// re-learned around it.
func watchVoiceLatency(guildID, botID snowflake.ID, latency *ackLatency) func() {
	m, ok := regionMetrics.Load(botID)
	if !ok {
		return func() {}
	}
	metrics := m.(*telemetry.VoiceRegionMetrics)
	return metrics.SetLatencySource(guildID, botID, latency.read)
}
