package opus

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

var scheduleStart = time.Unix(1_000_000, 0)

// Lateness is what the pipeline-latency histogram cannot show: a frame waits
// past the 60 ms queue cap only when its tick fires late, and the tick-duration
// histogram measures the work, not the wake-up.
func TestTickLatenessIsMeasuredAgainstTheBoundary(t *testing.T) {
	s := newTickSchedule(scheduleStart)

	if got := s.lateness(scheduleStart.Add(3 * time.Millisecond)); got != 3*time.Millisecond {
		t.Errorf("lateness = %v, want 3ms", got)
	}
}

func TestTickFiringEarlyIsNotLate(t *testing.T) {
	s := newTickSchedule(scheduleStart)

	if got := s.lateness(scheduleStart.Add(-time.Millisecond)); got != 0 {
		t.Errorf("lateness = %v, want 0 for a tick that fired early", got)
	}
}

func TestNextTickWaitsForTheFollowingBoundary(t *testing.T) {
	s := newTickSchedule(scheduleStart)

	if got := s.next(scheduleStart.Add(2 * time.Millisecond)); got != 18*time.Millisecond {
		t.Errorf("wait = %v, want 18ms to the next 20 ms boundary", got)
	}
	if got := s.lateness(scheduleStart.Add(25 * time.Millisecond)); got != 5*time.Millisecond {
		t.Errorf("lateness = %v, want 5ms past the 20 ms boundary", got)
	}
}

// A tick that overran a whole period resumes on the next future boundary
// instead of firing catch-up ticks, and lateness is then measured against that
// boundary — not accumulated from the slot that was skipped.
func TestNextTickSkipsBoundariesAlreadyPassed(t *testing.T) {
	s := newTickSchedule(scheduleStart)

	if got := s.next(scheduleStart.Add(45 * time.Millisecond)); got != 15*time.Millisecond {
		t.Errorf("wait = %v, want 15ms to the 60 ms boundary", got)
	}
	if got := s.lateness(scheduleStart.Add(60 * time.Millisecond)); got != 0 {
		t.Errorf("lateness = %v, want 0 at the 60 ms boundary", got)
	}
}

func TestNextTickNeverWaitsLessThanAMillisecond(t *testing.T) {
	s := newTickSchedule(scheduleStart)

	if got := s.next(scheduleStart.Add(19*time.Millisecond + 500*time.Microsecond)); got != time.Millisecond {
		t.Errorf("wait = %v, want the 1ms floor", got)
	}
}

// The wiring the helpers above cannot show: a running mixer records one
// lateness observation per tick, tagged with its guild.
func TestRunningMixerRecordsTickLateness(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("mixer_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m, err := NewMixer(metrics.Opus.For("guild-1"))
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*mixerFrameDur)
	defer cancel()
	m.Run(ctx)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.mixer.tick.lateness" {
				continue
			}
			h, ok := md.Data.(metricdata.Histogram[float64])
			if !ok || len(h.DataPoints) != 1 {
				t.Fatalf("gdc.mixer.tick.lateness = %T with %d points, want one histogram point", md.Data, len(h.DataPoints))
			}
			if g, _ := h.DataPoints[0].Attributes.Value("guild_id"); g.AsString() != "guild-1" {
				t.Errorf("guild_id = %q, want guild-1", g.AsString())
			}
			if h.DataPoints[0].Count < 2 {
				t.Errorf("lateness observations = %d over 5 ticks, want at least 2", h.DataPoints[0].Count)
			}
			return
		}
	}
	t.Fatal("gdc.mixer.tick.lateness was never recorded")
}
