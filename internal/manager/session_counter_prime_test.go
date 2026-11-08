package manager

import (
	"context"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func counterByGuild(t *testing.T, reader *sdkmetric.ManualReader, name string) map[snowflake.ID]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[snowflake.ID]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s: unexpected data type %T", name, md.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("guild_id"))
				id, err := snowflake.Parse(v.AsString())
				if err != nil {
					t.Fatalf("guild_id %q: %v", v.AsString(), err)
				}
				out[id] = dp.Value
			}
		}
	}
	return out
}

// A counter series that is born holding 1 has no earlier sample for
// Prometheus to subtract, so increase() reports 0 for the guild's first raid
// since the process started: on 2026-11-08 a guild with a 9-hour raid showed
// no sessions. The series has to be exported at 0 before any raid starts.
func TestSessionCounters_ExistAtZeroBeforeFirstRaid(t *testing.T) {
	t.Parallel()
	h := newBotOnlineHarness(t)
	if err := h.svc.metrics.Bot.RegisterGuildInfo(h.svc.observeGuildInfo); err != nil {
		t.Fatalf("RegisterGuildInfo: %v", err)
	}

	h.joinGuild(testGuildID)
	if _, cached := h.client.Caches.Guild(testGuildID); !cached {
		t.Fatalf("guild %s is not in the owner cache after joining", testGuildID)
	}

	for _, name := range []string{"gdc.voice.session.start.total", "gdc.voice.session.stop.total"} {
		got, exported := counterByGuild(t, h.reader, name)[testGuildID]
		if !exported {
			t.Fatalf("%s has no series for guild %s before its first raid", name, testGuildID)
		}
		if got != 0 {
			t.Errorf("%s = %d before any raid, want 0", name, got)
		}
	}

	h.svc.metrics.ForGuild(context.Background(), testGuildID).SessionStarted(2, "one_caller")
	if got := counterByGuild(t, h.reader, "gdc.voice.session.start.total")[testGuildID]; got != 1 {
		t.Errorf("start total after the first raid = %d, want 1", got)
	}
}
