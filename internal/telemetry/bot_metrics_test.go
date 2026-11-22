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
// metrics. gdc.command.duration is recorded after the handler returns, so on its
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
	if _, ok := commandDuration(t, reader); ok {
		t.Error("gdc.command.duration must stay absent while the handler is still running")
	}

	// Once it completes the two agree, so the difference is a live gauge of
	// handlers that went in and never came out.
	b.RecordCommand(ctx, "start", "100", 0.5)

	started, _ = collectSum(t, reader, "gdc.command.started.total")
	completed := commandDurationPoint(t, reader).Count
	if uint64(started) != completed {
		t.Errorf("started (%d) and completed (%d) must agree after the handler returns", started, completed)
	}
}

func commandDurationPoint(t *testing.T, reader *sdkmetric.ManualReader) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	point, ok := commandDuration(t, reader)
	if !ok {
		t.Fatal("gdc.command.duration was not emitted")
	}
	return point
}

// commandDuration returns the single gdc.command.duration point, and whether
// the instrument produced one.
func commandDuration(t *testing.T, reader *sdkmetric.ManualReader) (metricdata.HistogramDataPoint[float64], bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "gdc.command.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(hist.DataPoints) > 1 {
				t.Fatalf("gdc.command.duration: want one histogram point, got %T %+v", m.Data, m.Data)
			}
			if len(hist.DataPoints) == 0 {
				return metricdata.HistogramDataPoint[float64]{}, false
			}
			return hist.DataPoints[0], true
		}
	}
	return metricdata.HistogramDataPoint[float64]{}, false
}

func bucketOf(bounds []float64, seconds float64) int {
	for i, upper := range bounds {
		if seconds <= upper {
			return i
		}
	}
	return len(bounds)
}

// The histogram is in seconds, but with no boundaries of its own it took the
// SDK's defaults (0, 5, 10, 25 … 10000), which are sized for milliseconds. Every
// command in a week landed in the single bucket up to 5 s, so the dashboard's
// p50, p95 and p99 were interpolation across 0–5 s and said nothing. What the
// metric has to answer is whether commands beat Discord's 3 s acknowledgement
// deadline, so a prompt command, one that just made it and one that missed must
// not share a bucket.
func TestCommandDurationBucketsSeparateWithinFromPastTheDeadline(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	var b BotMetrics
	if err := b.init(meter); err != nil {
		t.Fatalf("init: %v", err)
	}

	const prompt, justInTime, late = 0.04, 2.5, 3.5
	ctx := context.Background()
	for _, seconds := range []float64{prompt, justInTime, late} {
		b.RecordCommand(ctx, "start", "100", seconds)
	}

	bounds := commandDurationPoint(t, reader).Bounds
	promptBucket, justInTimeBucket, lateBucket := bucketOf(bounds, prompt), bucketOf(bounds, justInTime), bucketOf(bounds, late)
	if promptBucket == justInTimeBucket {
		t.Errorf("a %v s and a %v s command share a bucket; boundaries %v cannot tell them apart", prompt, justInTime, bounds)
	}
	if justInTimeBucket == lateBucket {
		t.Errorf("a %v s command, inside Discord's 3 s deadline, and a %v s one, past it, share a bucket; boundaries %v", justInTime, late, bounds)
	}
}
