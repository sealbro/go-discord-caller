package pool

import (
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
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
