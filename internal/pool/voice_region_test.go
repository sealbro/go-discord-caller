package pool

import (
	"context"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// regionSeries counts the exported gdc.voice.server.region data points.
func regionSeries(t *testing.T, reader *sdkmetric.ManualReader) int {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.voice.server.region" {
				continue
			}
			gauge, ok := md.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("gdc.voice.server.region is %T, want Gauge[int64]", md.Data)
			}
			return len(gauge.DataPoints)
		}
	}
	return 0
}

// The region is live state, and the GuildVoiceLeave event that retires it is
// exactly the one this package already assumes can go missing — a degraded
// conn, a gateway that dies mid-teardown (see AudioSenderRegistry). When it
// does, the bot is reported as connected to that region for the rest of the
// process's life, so teardown must retire the series itself.
func TestLeaveForgetsTheVoiceRegion(t *testing.T) {
	const (
		botID     = snowflake.ID(1430511050704289835)
		guildID   = snowflake.ID(1529403555667251310)
		channelID = snowflake.ID(1529403555667251311)
	)

	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("pool_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Registering the listeners is what a client build does; the bot then lands
	// on a region.
	_ = VoiceRegionListeners(botID, &m.Voice)
	m.Voice.SetRegion(guildID, botID, "c-ams18-50a8bfa.discord.media:443")
	if got := regionSeries(t, reader); got != 1 {
		t.Fatalf("series after joining = %d, want 1", got)
	}

	// An already-cancelled context makes the stub conn's Close return at once;
	// what matters here is that Leave ran, not how long disgo waited.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewGuildVoice(&hangingVoiceManager{conn: newHangingConn()}, channelID).ForBot(botID).Leave(ctx, guildID)

	if got := regionSeries(t, reader); got != 0 {
		t.Errorf("series after leaving = %d, want 0 — the bot is reported in a region it left", got)
	}
}
