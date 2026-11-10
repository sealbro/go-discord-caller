package telemetry

import (
	"context"
	"math"
	"runtime"
	"runtime/metrics"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/buildinfo"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestProcessMetricsExportRuntimeAndCPU(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	runtime.GC()
	if err := startProcessMetrics(mp); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	for _, name := range []string{"gdc.build.info", "gdc.process.cpu.time", "go.memory.used", "go.goroutine.count", "gdc.gc.pause.duration", "gdc.sched.latency.over"} {
		if !seen[name] {
			t.Errorf("metric %q was not exported; got %v", name, seen)
		}
	}
}

func TestResidentBytesReadsSecondStatmField(t *testing.T) {
	got, err := residentBytes([]byte("54321 12345 678 9 0 4321 0\n"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(12345 * 4096); got != want {
		t.Errorf("residentBytes = %d, want %d", got, want)
	}
}

func TestResidentBytesRejectsTruncatedStatm(t *testing.T) {
	if _, err := residentBytes([]byte("54321"), 4096); err == nil {
		t.Error("expected an error for a statm line without a resident field")
	}
}

func TestBuildInfoCarriesStampedVersion(t *testing.T) {
	prev := buildinfo.Version
	buildinfo.Version = "1.2.3"
	t.Cleanup(func() { buildinfo.Version = prev })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := startProcessMetrics(mp); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gdc.build.info" {
				continue
			}
			points := m.Data.(metricdata.Gauge[int64]).DataPoints
			if len(points) != 1 {
				t.Fatalf("build info points = %d, want 1", len(points))
			}
			if v, _ := points[0].Attributes.Value(attribute.Key("version")); v.AsString() != "1.2.3" {
				t.Errorf("version = %q, want the stamped 1.2.3", v.AsString())
			}
			return
		}
	}
	t.Fatal("gdc.build.info was not exported")
}

func TestGCPauseRecorderRecordsEachPauseOnce(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := newGCPauseRecorder(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	hist := &metrics.Float64Histogram{
		Counts:  []uint64{2, 0, 1},
		Buckets: []float64{0, 0.0001, 0.001, math.Inf(1)},
	}
	rec.recordNew(context.Background(), hist)
	hist.Counts = []uint64{2, 1, 1}
	rec.recordNew(context.Background(), hist)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	dp := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64]).DataPoints[0]
	if dp.Count != 4 {
		t.Errorf("recorded %d pauses, want 4 (2 + 1 + 1 new)", dp.Count)
	}
	if want := 0.1*2 + 1 + 1; math.Abs(dp.Sum-want) > 1e-9 {
		t.Errorf("sum = %v ms, want %v", dp.Sum, want)
	}
}

func TestCountAtLeastSkipsBucketsStraddlingTheThreshold(t *testing.T) {
	h := &metrics.Float64Histogram{
		Counts:  []uint64{5, 3, 2, 1},
		Buckets: []float64{0, 0.0005, 0.001, 0.005, math.Inf(1)},
	}
	for seconds, want := range map[float64]uint64{0: 11, 0.001: 3, 0.0007: 3, 0.005: 1, 0.02: 0} {
		if got := countAtLeast(h, seconds); got != want {
			t.Errorf("countAtLeast(%v) = %d, want %d", seconds, got, want)
		}
	}
}
