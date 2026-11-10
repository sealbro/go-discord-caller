package telemetry

import (
	"context"
	"math"
	"runtime/metrics"

	"go.opentelemetry.io/otel/metric"
)

const gcPauseSample = "/sched/pauses/total/gc:seconds"

// gcPauseRecorder turns the runtime's cumulative stop-the-world pause
// histogram into per-collection observations, so GC pauses can be compared
// bucket for bucket with gdc.mixer.tick.lateness.
type gcPauseRecorder struct {
	duration metric.Float64Histogram
	sample   []metrics.Sample
	seen     []uint64
}

func newGCPauseRecorder(meter metric.Meter) (*gcPauseRecorder, error) {
	duration, err := meter.Float64Histogram("gdc.gc.pause.duration",
		metric.WithDescription("Stop-the-world garbage collection pauses; compare with gdc.mixer.tick.lateness."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.25, 1, 5, 20),
	)
	if err != nil {
		return nil, err
	}
	return &gcPauseRecorder{
		duration: duration,
		sample:   []metrics.Sample{{Name: gcPauseSample}},
	}, nil
}

func (r *gcPauseRecorder) record(ctx context.Context) {
	metrics.Read(r.sample)
	if r.sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		return
	}
	r.recordNew(ctx, r.sample[0].Value.Float64Histogram())
}

func (r *gcPauseRecorder) recordNew(ctx context.Context, h *metrics.Float64Histogram) {
	if r.seen == nil {
		r.seen = make([]uint64, len(h.Counts))
	}
	for i, count := range h.Counts {
		pauseMs := bucketCeilingSeconds(h.Buckets, i) * 1000
		for range count - r.seen[i] {
			r.duration.Record(ctx, pauseMs)
		}
		r.seen[i] = count
	}
}

// bucketCeilingSeconds is the upper bound of bucket i, or its lower bound for
// the open-ended last bucket.
func bucketCeilingSeconds(buckets []float64, i int) float64 {
	if upper := buckets[i+1]; !math.IsInf(upper, 1) {
		return upper
	}
	return buckets[i]
}
