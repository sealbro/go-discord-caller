package telemetry

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectSum returns the single data point of the named Int64 sum, and whether
// the instrument produced any data at all.
func collectSum(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				return 0, false
			}
			return sum.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// A slash command whose handler blocks forever must still be visible in the
// metrics. gdc.command.total is recorded after the handler returns, so on its
// own it cannot distinguish "wedged handler" from "nobody ran the command" —
// the 2026-09-22 outage, where /start and /stop hung for a day behind a held
// manager lock and produced neither a metric nor a log line.
func TestRecordCommandStart_MakesWedgedHandlerVisible(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	var b BotMetrics
	if err := b.init(meter); err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx := context.Background()
	b.RecordCommandStart(ctx, "start", "100") // handler entered, then blocks

	started, ok := collectSum(t, reader, "gdc.command.started.total")
	if !ok {
		t.Fatal("gdc.command.started.total must be emitted on handler entry")
	}
	if started != 1 {
		t.Errorf("want 1 started, got %d", started)
	}
	if _, ok := collectSum(t, reader, "gdc.command.total"); ok {
		t.Error("gdc.command.total must stay absent while the handler is still running")
	}

	// Once it completes the two agree, so the difference is a live gauge of
	// handlers that went in and never came out.
	b.RecordCommand(ctx, "start", "100", 0.5)

	started, _ = collectSum(t, reader, "gdc.command.started.total")
	completed, ok := collectSum(t, reader, "gdc.command.total")
	if !ok {
		t.Fatal("gdc.command.total must be emitted on completion")
	}
	if started != completed {
		t.Errorf("started (%d) and completed (%d) must agree after the handler returns", started, completed)
	}
}
