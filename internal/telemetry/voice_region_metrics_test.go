package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestParseVoiceRegion(t *testing.T) {
	tests := map[string]string{
		// Observed on a live raid, 2026-09-29: a "c-" prefix, the region and
		// its server number, then a hex instance id.
		"c-ams18-50a8bfa.discord.media:443": "c-ams",
		"c-ams13-8e0de.discord.media:443":   "c-ams",
		"c-ams22-1fa39d.discord.media:443":  "c-ams",

		// Constructed, not observed: a region whose own name contains a
		// hyphen must survive intact.
		"c-us-east18-50a8bfa.discord.media:443": "c-us-east",

		// Older name-plus-digits endpoints. Not seen in the wild since the
		// "c-" format appeared; kept so the fallback branch stays covered.
		"rotterdam9231.discord.media:443": "rotterdam",
		"us-east4823.discord.media:443":   "us-east",

		"": "",
	}

	for endpoint, want := range tests {
		if got := ParseVoiceRegion(endpoint); got != want {
			t.Errorf("ParseVoiceRegion(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestVoiceRegionMetricsTracksAndForgets(t *testing.T) {
	var m VoiceRegionMetrics
	m.regions = make(map[voiceRegionKey]string)

	guildID, botID := snowflake.ID(1), snowflake.ID(2)
	key := voiceRegionKey{guildID: guildID, botID: botID}

	m.SetRegion(guildID, botID, "c-ams18-50a8bfa.discord.media:443")
	if got := m.regions[key]; got != "c-ams" {
		t.Fatalf("after SetRegion: got %q, want %q", got, "c-ams")
	}

	// A reconnect to a different media server in the same region must not
	// produce a second series.
	m.SetRegion(guildID, botID, "c-ams22-1fa39d.discord.media:443")
	if len(m.regions) != 1 {
		t.Fatalf("reconnect within a region: got %d series, want 1", len(m.regions))
	}

	// An unparseable endpoint leaves the last known region in place.
	m.SetRegion(guildID, botID, "")
	if got := m.regions[key]; got != "c-ams" {
		t.Fatalf("after empty endpoint: got %q, want %q", got, "c-ams")
	}

	m.ForgetRegion(guildID, botID)
	if len(m.regions) != 0 {
		t.Fatalf("after ForgetRegion: got %d series, want 0", len(m.regions))
	}
}

func collectVoiceLatency(t *testing.T, reader *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "gdc.voice.server.latency" {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("gdc.voice.server.latency is %T, want a float gauge", m.Data)
			}
			for _, dp := range gauge.DataPoints {
				region, _ := dp.Attributes.Value("region")
				got[region.AsString()] = dp.Value
			}
		}
	}
	return got
}

func TestVoiceServerLatencyIsReportedPerRegion(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	var m VoiceRegionMetrics
	if err := m.init(meter); err != nil {
		t.Fatalf("init: %v", err)
	}

	guildID, botID := snowflake.ID(1), snowflake.ID(2)
	m.SetLatencySource(guildID, botID, func() time.Duration { return 42 * time.Millisecond })

	// The region arrives with VOICE_SERVER_UPDATE; until then the latency has
	// no label to be filed under.
	if got := collectVoiceLatency(t, reader); len(got) != 0 {
		t.Fatalf("before the region is known: got %v, want nothing", got)
	}

	m.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")
	got := collectVoiceLatency(t, reader)
	if got["c-fra"] != 42 {
		t.Fatalf("after SetRegion: got %v, want c-fra=42", got)
	}

	m.ForgetRegion(guildID, botID)
	if got := collectVoiceLatency(t, reader); len(got) != 0 {
		t.Fatalf("after ForgetRegion: got %v, want nothing", got)
	}
}

// A source with nothing measured yet reports zero or less, which must not be
// exported as a measurement.
func TestVoiceServerLatencySkipsUnmeasuredConnections(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	var m VoiceRegionMetrics
	if err := m.init(meter); err != nil {
		t.Fatalf("init: %v", err)
	}

	guildID, botID := snowflake.ID(1), snowflake.ID(2)
	m.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")
	forget := m.SetLatencySource(guildID, botID, func() time.Duration { return -3 * time.Second })

	if got := collectVoiceLatency(t, reader); len(got) != 0 {
		t.Fatalf("negative latency: got %v, want nothing", got)
	}

	forget()
	m.SetLatencySource(guildID, botID, func() time.Duration { return 0 })
	if got := collectVoiceLatency(t, reader); len(got) != 0 {
		t.Fatalf("zero latency: got %v, want nothing", got)
	}
}

func TestForgettingAReplacedLatencySourceKeepsTheNewOne(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	var m VoiceRegionMetrics
	if err := m.init(meter); err != nil {
		t.Fatalf("init: %v", err)
	}

	guildID, botID := snowflake.ID(1), snowflake.ID(2)
	m.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")
	forgetOld := m.SetLatencySource(guildID, botID, func() time.Duration { return 10 * time.Millisecond })
	m.SetLatencySource(guildID, botID, func() time.Duration { return 20 * time.Millisecond })

	forgetOld()
	if got := collectVoiceLatency(t, reader); got["c-fra"] != 20 {
		t.Fatalf("after the old source was forgotten: got %v, want c-fra=20", got)
	}
}
