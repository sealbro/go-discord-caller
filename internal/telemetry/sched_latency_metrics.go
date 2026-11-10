package telemetry

import (
	"context"
	"runtime/metrics"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const schedLatencySample = "/sched/latencies:seconds"

// schedLatencyThresholdsMs are the waits, in milliseconds, that goroutine
// schedulings are counted against; 0 counts every scheduling.
var schedLatencyThresholdsMs = []float64{0, 1, 5, 20}

// registerSchedLatencyMetrics exports how often a runnable goroutine waited
// at least each threshold for a CPU. A scheduling is far too frequent to
// record one by one, so the runtime's cumulative histogram is read instead.
func registerSchedLatencyMetrics(meter metric.Meter) error {
	over, err := meter.Int64ObservableCounter("gdc.sched.latency.over",
		metric.WithDescription("Goroutine schedulings that waited at least over_ms before running; over_ms=0 counts all of them. Explains gdc.mixer.tick.lateness caused by CPU starvation."),
	)
	if err != nil {
		return err
	}
	sample := []metrics.Sample{{Name: schedLatencySample}}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		metrics.Read(sample)
		if sample[0].Value.Kind() != metrics.KindFloat64Histogram {
			return nil
		}
		h := sample[0].Value.Float64Histogram()
		for _, ms := range schedLatencyThresholdsMs {
			o.ObserveInt64(over, int64(countAtLeast(h, ms/1000)),
				metric.WithAttributes(attribute.String("over_ms", strconv.FormatFloat(ms, 'f', -1, 64))))
		}
		return nil
	}, over)
	return err
}

// countAtLeast counts the observations in buckets that begin at or above
// seconds, so a bucket straddling the threshold is not counted.
func countAtLeast(h *metrics.Float64Histogram, seconds float64) uint64 {
	var total uint64
	for i, count := range h.Counts {
		if h.Buckets[i] >= seconds {
			total += count
		}
	}
	return total
}
