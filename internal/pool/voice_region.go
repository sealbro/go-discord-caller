package pool

import (
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
