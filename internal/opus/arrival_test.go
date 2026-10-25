package opus

import (
	"context"
	"testing"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func receiverWithMetrics(t *testing.T, install FanoutInstall) (*VoiceReceiver, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("arrival_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	handle := NewFanoutHandle()
	handle.Install(install)
	return NewVoiceReceiver(0, nil, metrics.Opus.For("guild-1"), handle), reader
}

// histogramCounts returns the per-bucket counts of the single data point of the
// named histogram, the last entry being the +Inf bucket, or nil when nothing
// was recorded.
func histogramCounts(t *testing.T, reader *sdkmetric.ManualReader, name string) (bounds []float64, counts []uint64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			switch h := md.Data.(type) {
			case metricdata.Histogram[float64]:
				if len(h.DataPoints) != 1 {
					t.Fatalf("%s has %d points, want 1", name, len(h.DataPoints))
				}
				return h.DataPoints[0].Bounds, h.DataPoints[0].BucketCounts
			case metricdata.Histogram[int64]:
				if len(h.DataPoints) != 1 {
					t.Fatalf("%s has %d points, want 1", name, len(h.DataPoints))
				}
				return h.DataPoints[0].Bounds, h.DataPoints[0].BucketCounts
			default:
				t.Fatalf("%s = %T, want a histogram", name, md.Data)
			}
		}
	}
	return nil, nil
}

func total(counts []uint64) (n uint64) {
	for _, c := range counts {
		n += c
	}
	return n
}

func receiveAt(t *testing.T, r *VoiceReceiver, userID snowflake.ID, timestamp uint32, opus []byte) {
	t.Helper()
	if err := r.ReceiveOpusFrame(userID, &voice.Packet{Timestamp: timestamp, Opus: opus}); err != nil {
		t.Fatalf("ReceiveOpusFrame: %v", err)
	}
}

var copyFrame = []byte{0xFC, 1, 2}

func copyModeReceiver(t *testing.T) (*VoiceReceiver, *sdkmetric.ManualReader) {
	return receiverWithMetrics(t, FanoutInstall{OpusTargets: []chan<- []byte{make(chan []byte, 64)}})
}

// Two packets one frame apart by RTP timestamp but delivered back to back are
// a clump: the second one waited somewhere and arrived ~0 ms after the first
// instead of ~20 ms.
func TestReceiveIntervalRecordsAClumpOfConsecutiveFrames(t *testing.T) {
	r, reader := copyModeReceiver(t)

	receiveAt(t, r, 7, 1000, copyFrame)
	receiveAt(t, r, 7, 1000+960, copyFrame)

	bounds, counts := histogramCounts(t, reader, "gdc.opus.receive.interval")
	if total(counts) != 1 {
		t.Fatalf("interval counts = %v, want exactly one observation", counts)
	}
	if counts[0] != 1 {
		t.Errorf("interval counts = %v over bounds %v, want the back-to-back pair in the lowest bucket", counts, bounds)
	}
}

// Discord sends nothing while a user is silent, so the gap across a pause is
// speech timing, not delivery jitter, and would swamp the distribution.
func TestReceiveIntervalSkipsGapsAcrossSilence(t *testing.T) {
	r, reader := copyModeReceiver(t)

	receiveAt(t, r, 7, 1000, copyFrame)
	receiveAt(t, r, 7, 1000+50*960, copyFrame)

	if _, counts := histogramCounts(t, reader, "gdc.opus.receive.interval"); total(counts) != 0 {
		t.Errorf("interval counts = %v, want nothing recorded across a pause", counts)
	}
}

func TestReceiveIntervalIsTrackedPerUser(t *testing.T) {
	r, reader := copyModeReceiver(t)

	receiveAt(t, r, 7, 1000, copyFrame)
	receiveAt(t, r, 8, 1000+960, copyFrame)
	receiveAt(t, r, 7, 1000+960, copyFrame)

	if _, counts := histogramCounts(t, reader, "gdc.opus.receive.interval"); total(counts) != 1 {
		t.Errorf("interval counts = %v, want one observation for user 7 and none for user 8's first packet", counts)
	}
}

func TestReceiveIntervalHandlesTimestampWraparound(t *testing.T) {
	r, reader := copyModeReceiver(t)

	beforeWrap := ^uint32(0) - 100
	receiveAt(t, r, 7, beforeWrap, copyFrame)
	receiveAt(t, r, 7, beforeWrap+960, copyFrame)

	if _, counts := histogramCounts(t, reader, "gdc.opus.receive.interval"); total(counts) != 1 {
		t.Errorf("interval counts = %v, want one observation across the RTP timestamp wrap", counts)
	}
}

func TestReceiveIntervalForgetsACleanedUpUser(t *testing.T) {
	r, reader := copyModeReceiver(t)

	receiveAt(t, r, 7, 1000, copyFrame)
	r.CleanupUser(7)
	receiveAt(t, r, 7, 1000+960, copyFrame)

	if _, counts := histogramCounts(t, reader, "gdc.opus.receive.interval"); total(counts) != 0 {
		t.Errorf("interval counts = %v, want nothing recorded after the user was cleaned up", counts)
	}
}

// Each frame fed into a mixer input records how many frames were already
// waiting there; a full buffer (3 ahead) is the feed that evicts one.
func TestSourceDepthRecordsFramesAlreadyBuffered(t *testing.T) {
	frames := generateTestFrames(t, 440, 4, testAmplitude)
	src := NewSourceBuffer(nil)
	r, reader := receiverWithMetrics(t, FanoutInstall{
		SourceTargets: map[snowflake.ID][]*SourceBuffer{7: {src}},
	})

	for i, f := range frames {
		receiveAt(t, r, 7, uint32(i*960), f.opus)
	}

	bounds, counts := histogramCounts(t, reader, "gdc.opus.source.depth")
	want := []uint64{1, 1, 1, 1, 0}
	if len(counts) != len(want) {
		t.Fatalf("depth bounds = %v, want 0, 1, 2, 3", bounds)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatalf("depth counts = %v over bounds %v, want one feed each at 0, 1, 2 and 3 ahead", counts, bounds)
		}
	}
}
