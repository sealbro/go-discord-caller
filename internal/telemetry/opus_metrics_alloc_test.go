package telemetry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// The recorders run once per audio frame or mixer tick, so a single allocation
// in one of them is 50 per second for every mixer and every speaking user.
func TestHotPathRecordersDoNotAllocate(t *testing.T) {
	metrics, err := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())).Meter("alloc_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	rec := metrics.Opus.For("guild-1")
	drop := metrics.Session.FrameDropper(context.Background(), 1, DropPathMixer)
	speakerDrop := metrics.Session.SpeakerFrameDropper(1, DropPathDirect, 2)

	for name, record := range map[string]func(){
		"RecordTick":             func() { rec.RecordTick(0.2) },
		"RecordTickLateness":     func() { rec.RecordTickLateness(0.2) },
		"RecordPipelineLatency":  func() { rec.RecordPipelineLatency(20) },
		"RecordReceive":          func() { rec.RecordReceive(0.2) },
		"RecordReceiveInterval":  func() { rec.RecordReceiveInterval(20) },
		"RecordProvide":          func() { rec.RecordProvide(0.2) },
		"RecordCopyQueue":        func() { rec.RecordCopyQueue(1) },
		"RecordSourceDepth":      func() { rec.RecordSourceDepth(1) },
		"RecordSourceFramesLost": func() { rec.RecordSourceFramesLost() },
		"RecordDecryptDuration":  func() { metrics.Dave.RecordDecryptDuration("bot-1", time.Millisecond) },
		"FrameDropper":           drop,
		"SpeakerFrameDropper":    speakerDrop,
	} {
		record()
		if allocs := testing.AllocsPerRun(1000, record); allocs != 0 {
			t.Errorf("%s allocates %.0f times per call, want 0", name, allocs)
		}
	}
}
