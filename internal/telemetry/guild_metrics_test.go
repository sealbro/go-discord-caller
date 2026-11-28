package telemetry

import (
	"context"
	"reflect"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"
)

// Raid audio builds its mixers from OpusMetrics, so it must be the guild's own
// recorder: one without metrics behind it records nothing.
func TestGuildMetrics_OpusMetricsIsTheGuildsRecorder(t *testing.T) {
	metrics, err := NewMetrics(noop.NewMeterProvider().Meter("guild_metrics_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	g := metrics.ForGuild(context.Background(), snowflake.ID(768))

	got := g.OpusMetrics()
	if got.m == nil || !reflect.DeepEqual(got, g.Opus) {
		t.Errorf("OpusMetrics() = %+v, want the guild's recorder %+v", got, g.Opus)
	}
}
