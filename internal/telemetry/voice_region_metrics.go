package telemetry

import (
	"context"
	"strings"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// VoiceRegionMetrics reports which Discord voice region each bot's voice
// connection landed on, as an info gauge whose value is always 1.
//
// It is its own series rather than a label on the pipeline histograms because
// the region only becomes known when Discord answers VOICE_SERVER_UPDATE,
// which arrives after the OpusRecorder attribute sets are built — and those
// sets are deliberately pre-baked to keep the per-frame path allocation-free.
// Join it in PromQL instead:
//
//	histogram_quantile(0.95, sum by (le, region) (
//	  rate(gdc_mixer_pipeline_latency_milliseconds_bucket[5m])
//	  * on (guild_id) group_left (region)
//	    clamp_max(topk by (guild_id) (1, timestamp(gdc_voice_server_region)), 1)
//	))
//
// The right side has to collapse to exactly one series per guild or group_left
// rejects the join, and this metric is per bot: a guild whose bots landed in
// different regions has several, as does any guild for the few minutes after a
// region changes, while the previous series is still inside the lookback
// window. topk on the sample timestamp picks the freshest, and clamp_max turns
// that timestamp back into the 1 the join expects. The consequence is that a
// guild genuinely split across regions is reported under one of them — the
// histograms are per guild, so there is no exact answer to give.
type VoiceRegionMetrics struct {
	gauge   metric.Int64ObservableGauge
	mu      sync.RWMutex
	regions map[voiceRegionKey]string
}

type voiceRegionKey struct {
	guildID snowflake.ID
	botID   snowflake.ID
}

func (v *VoiceRegionMetrics) init(meter metric.Meter) error {
	v.regions = make(map[voiceRegionKey]string)

	gauge, err := meter.Int64ObservableGauge("gdc.voice.server.region",
		metric.WithDescription("Info gauge for the Discord voice region a bot's voice connection landed on; value is always 1. Labels: guild_id, bot_id, region."),
	)
	if err != nil {
		return err
	}
	v.gauge = gauge

	_, err = meter.RegisterCallback(v.observe, gauge)
	return err
}

func (v *VoiceRegionMetrics) observe(_ context.Context, o metric.Observer) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	for key, region := range v.regions {
		o.ObserveInt64(v.gauge, 1, metric.WithAttributes(
			attribute.String("guild_id", key.guildID.String()),
			attribute.String("bot_id", key.botID.String()),
			attribute.String("region", region),
		))
	}
	return nil
}

// SetRegion records the region carried by a VOICE_SERVER_UPDATE endpoint.
// An endpoint that yields no region is ignored, so a momentary gap leaves the
// last known region in place until ForgetRegion tears the series down.
func (v *VoiceRegionMetrics) SetRegion(guildID, botID snowflake.ID, endpoint string) {
	region := ParseVoiceRegion(endpoint)
	if region == "" {
		return
	}

	v.mu.Lock()
	v.regions[voiceRegionKey{guildID: guildID, botID: botID}] = region
	v.mu.Unlock()
}

// ForgetRegion drops the series for a bot that has left voice in a guild.
// Without it every guild the process has ever served would be reported for the
// lifetime of the process, as live state.
func (v *VoiceRegionMetrics) ForgetRegion(guildID, botID snowflake.ID) {
	v.mu.Lock()
	delete(v.regions, voiceRegionKey{guildID: guildID, botID: botID})
	v.mu.Unlock()
}

// ParseVoiceRegion extracts the region from a Discord voice endpoint such as
// "c-ams18-50a8bfa.discord.media:443", which yields "c-ams", or the older
// "rotterdam9231.discord.media:443", which yields "rotterdam".
//
// Everything from the first digit on names the individual media server — its
// number, and in the current format a hex instance id — and is dropped, since
// keeping it would mint a series per reconnect. Cutting there rather than at a
// hyphen means a region whose own name contains one survives without the
// parser having to know how many segments an endpoint has.
func ParseVoiceRegion(endpoint string) string {
	host, _, _ := strings.Cut(endpoint, ":")
	host, _, _ = strings.Cut(host, ".")

	if i := strings.IndexFunc(host, func(r rune) bool { return r >= '0' && r <= '9' }); i >= 0 {
		host = host[:i]
	}

	return strings.Trim(host, "-")
}
