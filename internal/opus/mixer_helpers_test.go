package opus

import (
	"bytes"
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hraban "github.com/hraban/opus"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

const (
	testFrameCount = MixerPCMBuf / MixerChannels // 960 samples per channel per 20ms frame
	testAmplitude  = 16000                       // safe for up to 2 simultaneous sources (2×16000=32000 < 32767)
	testAmplitude3 = 10000                       // safe for 3 simultaneous sources (3×10000=30000 < 32767)
)

type testFrame struct {
	pcm  []int16 // length MixerPCMBuf — pre-computed sine, used as mixer Frame.PCM
	opus []byte  // Opus-encoded version of pcm, used as mixer Frame.Opus
}

// sinePCM returns one stereo 20ms PCM frame of a sine at freq Hz with the given amplitude.
// frameIdx maintains phase continuity across consecutive frames.
func sinePCM(freq, frameIdx, amplitude int) []int16 {
	pcm := make([]int16, MixerPCMBuf)
	offset := frameIdx * testFrameCount
	for i := range testFrameCount {
		v := int16(float64(amplitude) * math.Sin(2*math.Pi*float64(freq)*float64(offset+i)/float64(MixerSampleRate)))
		pcm[i*2] = v
		pcm[i*2+1] = v
	}
	return pcm
}

// generateTestFrames pre-encodes n stereo 20ms sine frames at freq Hz with the given amplitude.
func generateTestFrames(t *testing.T, freq, n, amplitude int) []testFrame {
	t.Helper()
	enc, err := hraban.NewEncoder(MixerSampleRate, MixerChannels, hraban.AppVoIP)
	if err != nil {
		t.Fatalf("generateTestFrames: new encoder: %v", err)
	}
	raw := make([]byte, 4096)
	frames := make([]testFrame, n)
	for i := range frames {
		pcm := sinePCM(freq, i, amplitude)
		nn, err := enc.Encode(pcm, raw)
		if err != nil {
			t.Fatalf("generateTestFrames: encode frame %d: %v", i, err)
		}
		opus := make([]byte, nn)
		copy(opus, raw[:nn])
		frames[i] = testFrame{pcm: pcm, opus: opus}
	}
	return frames
}

// outputCollector accumulates Opus packets delivered via the mixer sink.
type outputCollector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *outputCollector) sink(pkt []byte) {
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	c.mu.Lock()
	c.frames = append(c.frames, cp)
	c.mu.Unlock()
}

func (c *outputCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// waitForN blocks until at least n total frames are collected, then returns the first n.
// Returns nil on timeout.
func (c *outputCollector) waitForN(n int, timeout time.Duration) [][]byte {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.frames)
		c.mu.Unlock()
		if got >= n {
			c.mu.Lock()
			result := make([][]byte, n)
			copy(result, c.frames[:n])
			c.mu.Unlock()
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// collectFrom blocks until at least baseline+n frames exist, then returns n frames
// starting at baseline. Returns nil on timeout.
func (c *outputCollector) collectFrom(baseline, n int, timeout time.Duration) [][]byte {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.frames)
		c.mu.Unlock()
		if got >= baseline+n {
			c.mu.Lock()
			result := make([][]byte, n)
			copy(result, c.frames[baseline:baseline+n])
			c.mu.Unlock()
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// newTestMixer creates a running Mixer with no-op metrics and a wired sink.
// Cancels the mixer and waits for Run to exit on test cleanup.
func newTestMixer(t *testing.T) (*Mixer, *outputCollector) {
	t.Helper()
	m, err := NewMixer(telemetry.OpusRecorder{})
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	col := &outputCollector{}
	m.SetSink(col.sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return m, col
}

// newSourceChan returns a buffered Frame channel sized to absorb timing jitter.
func newSourceChan() chan Frame { return make(chan Frame, 16) }

// startPump feeds frames from the pre-encoded slice into ch at 20ms cadence.
// Cycles through the slice indefinitely. Returns a stop func.
func startPump(ctx context.Context, ch chan<- Frame, frames []testFrame) (stop func()) {
	pumpCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(mixerFrameDur)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-pumpCtx.Done():
				return
			case <-ticker.C:
				f := frames[i%len(frames)]
				pcm := GetPCM()
				copy(pcm, f.pcm)
				opus := make([]byte, len(f.opus))
				copy(opus, f.opus)
				select {
				case ch <- Frame{PCM: pcm, Opus: opus, CreatedAt: time.Now()}:
				default:
					PutPCM(pcm) // channel full — discard rather than block (opus is heap-only, GC handles it)
				}
			}
		}
	}()
	return cancel
}

// decodeOpusFrames decodes a slice of Opus packets and returns concatenated PCM.
func decodeOpusFrames(t *testing.T, packets [][]byte) []int16 {
	t.Helper()
	dec, err := hraban.NewDecoder(MixerSampleRate, MixerChannels)
	if err != nil {
		t.Fatalf("decodeOpusFrames: new decoder: %v", err)
	}
	scratch := make([]int16, MixerPCMBuf)
	var out []int16
	for _, pkt := range packets {
		n, err := dec.Decode(pkt, scratch)
		if err != nil {
			t.Fatalf("decodeOpusFrames: decode: %v", err)
		}
		out = append(out, scratch[:n*MixerChannels]...)
	}
	return out
}

// rmsInt16 returns the root mean square of a PCM buffer.
func rmsInt16(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// maxAbsInt16 returns the peak absolute sample value in a PCM buffer.
func maxAbsInt16(pcm []int16) int {
	var peak int
	for _, v := range pcm {
		abs := int(v)
		if abs < 0 {
			abs = -abs
		}
		if abs > peak {
			peak = abs
		}
	}
	return peak
}

// matchesAnyFrame reports whether pkt is byte-equal to any frame's Opus field.
func matchesAnyFrame(pkt []byte, frames []testFrame) bool {
	for _, f := range frames {
		if bytes.Equal(pkt, f.opus) {
			return true
		}
	}
	return false
}

var testIDSeq atomic.Uint64

// newID returns a unique snowflake.ID safe for concurrent test use.
func newID() snowflake.ID { return snowflake.ID(testIDSeq.Add(1)) }
