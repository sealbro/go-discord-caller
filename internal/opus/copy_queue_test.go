package opus

import (
	"context"
	"testing"

	"github.com/disgoorg/disgo/voice"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// copyReceiver returns a receiver in copy mode, forwarding raw Opus to target,
// and the reader its metrics report to.
func copyReceiver(t *testing.T, target chan []byte) (*VoiceReceiver, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("copy_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	handle := NewFanoutHandle()
	handle.Install(FanoutInstall{OpusTargets: []chan<- []byte{target}})
	return NewVoiceReceiver(0, nil, metrics.Opus.For("guild-1"), handle), reader
}

// queueDepths returns the copy queue-depth histogram's buckets as
// frames-ahead → count.
func queueDepths(t *testing.T, reader *sdkmetric.ManualReader) map[float64]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.opus.copy.queue_depth" {
				continue
			}
			h, ok := md.Data.(metricdata.Histogram[int64])
			if !ok || len(h.DataPoints) != 1 {
				t.Fatalf("gdc.opus.copy.queue_depth = %T, want one int64 histogram point", md.Data)
			}
			got := map[float64]uint64{}
			p := h.DataPoints[0]
			for i, bound := range p.Bounds {
				if p.BucketCounts[i] > 0 {
					got[bound] = p.BucketCounts[i]
				}
			}
			return got
		}
	}
	return nil
}

// In copy mode a frame skips the mixer and waits in the speaker's output
// queue, which disgo drains one frame per 20 ms. The frames already queued
// ahead of it are therefore its wait, in 20 ms steps — the latency the mixer
// histograms never see.
func TestCopyPushRecordsFramesQueuedAhead(t *testing.T) {
	target := make(chan []byte, AudioChanBuf)
	target <- []byte{1}
	target <- []byte{2}
	receiver, reader := copyReceiver(t, target)

	if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{0xFC, 1, 2}}); err != nil {
		t.Fatalf("ReceiveOpusFrame: %v", err)
	}

	if got := queueDepths(t, reader); got[2] != 1 || len(got) != 1 {
		t.Errorf("queue depths = %v, want one frame recorded with 2 ahead", got)
	}
}

func TestCopyPushIntoAnEmptyQueueRecordsZero(t *testing.T) {
	target := make(chan []byte, AudioChanBuf)
	receiver, reader := copyReceiver(t, target)

	if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{0xFC, 1, 2}}); err != nil {
		t.Fatalf("ReceiveOpusFrame: %v", err)
	}

	if got := queueDepths(t, reader); got[0] != 1 || len(got) != 1 {
		t.Errorf("queue depths = %v, want one frame recorded with 0 ahead", got)
	}
}

// A frame that finds the queue full is dropped and counted as a drop; it never
// waited, so it must not skew the wait distribution.
func TestDroppedCopyFrameRecordsNoDepth(t *testing.T) {
	target := make(chan []byte, AudioChanBuf)
	for range AudioChanBuf {
		target <- []byte{0}
	}
	receiver, reader := copyReceiver(t, target)

	if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{0xFC, 1, 2}}); err != nil {
		t.Fatalf("ReceiveOpusFrame: %v", err)
	}

	if got := queueDepths(t, reader); len(got) != 0 {
		t.Errorf("queue depths = %v, want nothing recorded for a dropped frame", got)
	}
}
