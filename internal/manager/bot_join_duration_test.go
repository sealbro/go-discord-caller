package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// botJoins returns the gdc.voice.bot.join.duration sample counts by
// bot_kind/outcome.
func botJoins(t *testing.T, reader *sdkmetric.ManualReader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.voice.bot.join.duration" {
				continue
			}
			for _, dp := range md.Data.(metricdata.Histogram[float64]).DataPoints {
				kind, _ := dp.Attributes.Value("bot_kind")
				outcome, _ := dp.Attributes.Value("outcome")
				got[kind.AsString()+"/"+outcome.AsString()] += dp.Count
			}
		}
	}
	return got
}

func withRecordedMetrics(t *testing.T, f *callerRoleFixture) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("bot_join_duration_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	f.m.metrics = metrics
	return reader
}

func TestStartVoiceRaid_RecordsHowLongEachBotTookToJoin(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reader := withRecordedMetrics(t, f)

	_, cancel, err := startHostRaid(t, f)
	defer cancel()
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	got := botJoins(t, reader)
	if got["speaker/joined"] != 1 || got["owner/joined"] != 1 {
		t.Errorf("join samples = %v, want one speaker/joined and one owner/joined", got)
	}
}

func TestStartVoiceRaid_RecordsAFailedOwnerJoin(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reader := withRecordedMetrics(t, f)
	f.ownerConn.openErr = errors.New("join refused")

	_, cancel, _ := startHostRaid(t, f)
	defer cancel()

	if got := botJoins(t, reader); got["owner/failed"] != 1 {
		t.Errorf("join samples = %v, want one owner/failed", got)
	}
}

func TestJoinSession_RecordsGuestBotJoins(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reader := withRecordedMetrics(t, f)
	f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}

	got := botJoins(t, reader)
	if got["speaker/joined"] != 1 || got["owner/joined"] != 1 {
		t.Errorf("join samples = %v, want one speaker/joined and one owner/joined", got)
	}
}
