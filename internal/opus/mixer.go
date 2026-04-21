package opus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	hraban "github.com/hraban/opus"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// MixerSampleRate, MixerChannels, MixerPCMBuf are exported so callers that
// produce PCM for mixer inputs (e.g. decode-once fanout goroutines) can allocate
// correctly-sized buffers without duplicating these constants.
const (
	MixerSampleRate = mixerSampleRate
	MixerChannels   = mixerChannels
	MixerPCMBuf     = mixerPCMBuf
)

const (
	mixerSampleRate = 48000
	mixerChannels   = 2
	mixerFrameSize  = 960 // samples per channel for 20 ms at 48 kHz
	mixerPCMBuf     = mixerFrameSize * mixerChannels
	mixerFrameDur   = 20 * time.Millisecond
	// mixerOutputBuf is the output channel buffer depth (10 frames × 20 ms = 200 ms).
	// Frames are dropped silently when the consumer falls more than 200 ms behind.
	// Increase this if guest guilds experience frequent audio gaps under load.
	mixerOutputBuf = 10
)

// Frame carries one audio frame through a mixer input channel.
// PCM holds the pre-decoded samples used when multiple sources are mixed.
// Opus holds the original encoded packet used for single-source passthrough
// (when only one source is active the mixer forwards Opus directly, skipping re-encode).
// CreatedAt records when the frame was decoded in the fanout goroutine, used to
// measure end-to-end pipeline latency (decode → mixer input buffer → mix → encode).
type Frame struct {
	PCM       []int16
	Opus      []byte
	CreatedAt time.Time
}

// inputEntry holds a single mixer input. The channel carries Frame values
// produced by an upstream fanout goroutine.
type inputEntry struct {
	ch <-chan Frame
}

// Mixer receives Opus frames from multiple named input channels, mixes their PCM,
// and outputs a single re-encoded Opus stream on Output().
// Call Run to start the 20 ms tick loop; it blocks until ctx is cancelled and
// closes the output channel on return.
//
// A mixer can be paused when its destination channel has no listeners (e.g. no
// non-bot users). While paused, tick still drains input channels to prevent
// upstream backpressure, but skips mixing, encoding, and output — saving CPU.
type Mixer struct {
	mu     sync.Mutex
	inputs map[snowflake.ID]*inputEntry
	paused atomic.Bool

	out chan []byte
	enc *hraban.Encoder

	// Pre-allocated scratch buffers reused on every tick.
	// Only accessed from the single Run goroutine — no synchronisation needed.
	entriesBuf []*inputEntry
	framesBuf  []Frame // active frames collected each tick
	mixed      []int32
	pcm        []int16
	encodeBuf  []byte
}

// mixerComplexity is the Opus encoder complexity (0–10).
// Default is 9 (max quality); 3 gives ~60% CPU reduction vs default with
// acceptable voice quality for relay use cases.
const mixerComplexity = 3

// mixerInputDrainThreshold is the maximum number of queued frames per input
// (beyond the one just read) before the mixer drains to the latest.
// 4 frames × 20 ms = 80 ms of tolerated jitter before drain kicks in.
const mixerInputDrainThreshold = 4

// NewMixer creates a Mixer ready to accept inputs and run.
func NewMixer() (*Mixer, error) {
	enc, err := hraban.NewEncoder(mixerSampleRate, mixerChannels, hraban.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("mixer: new encoder: %w", err)
	}
	if err := enc.SetComplexity(mixerComplexity); err != nil {
		return nil, fmt.Errorf("mixer: set complexity: %w", err)
	}
	if err := enc.SetDTX(true); err != nil {
		return nil, fmt.Errorf("mixer: set dtx: %w", err)
	}
	// 16 kbps is sufficient for voice relay; default (~32 kbps) produces larger
	// frames than needed and wastes channel buffer space.
	if err := enc.SetBitrate(16000); err != nil {
		return nil, fmt.Errorf("mixer: set bitrate: %w", err)
	}
	// In-band FEC embeds redundancy so the receiver can reconstruct a lost packet
	// from the next one, avoiding audible glitches without retransmission.
	if err := enc.SetInBandFEC(true); err != nil {
		return nil, fmt.Errorf("mixer: set inband fec: %w", err)
	}
	// Tune FEC aggressiveness to match typical Discord packet loss (~5%).
	if err := enc.SetPacketLossPerc(5); err != nil {
		return nil, fmt.Errorf("mixer: set packet loss perc: %w", err)
	}
	return &Mixer{
		inputs:    make(map[snowflake.ID]*inputEntry),
		out:       make(chan []byte, mixerOutputBuf),
		enc:       enc,
		mixed:     make([]int32, mixerPCMBuf),
		pcm:       make([]int16, mixerPCMBuf),
		encodeBuf: make([]byte, 4096),
		framesBuf: make([]Frame, 0, 8),
	}, nil
}

// AddInput registers a new audio source identified by id.
// ch must carry Frame values produced by an upstream fanout goroutine.
func (m *Mixer) AddInput(id snowflake.ID, ch <-chan Frame) error {
	m.mu.Lock()
	m.inputs[id] = &inputEntry{ch: ch}
	m.mu.Unlock()
	return nil
}

// RemoveInput unregisters the audio source identified by id.
func (m *Mixer) RemoveInput(id snowflake.ID) {
	m.mu.Lock()
	delete(m.inputs, id)
	m.mu.Unlock()
}

// SetPaused controls whether the mixer is paused. While paused, tick drains
// input channels (preventing upstream backpressure) but skips mixing, encoding,
// and output. Use this to suspend mixers whose destination channel has no
// non-bot listeners.
func (m *Mixer) SetPaused(p bool) {
	m.paused.Store(p)
}

// Paused reports whether the mixer is currently paused.
func (m *Mixer) Paused() bool {
	return m.paused.Load()
}

// Output returns the channel carrying mixed Opus frames.
func (m *Mixer) Output() <-chan []byte {
	return m.out
}

// Run fires a 20 ms mix tick, waits for it to complete, then schedules the next
// one. Using time.Timer (reset after each tick) instead of time.Ticker prevents
// stale-frame buildup: if a tick takes longer than 20 ms under load, the next
// tick is deferred rather than queued immediately from a backlog.
// The timer accounts for tick processing time to prevent systematic drift:
// without correction each period is 20 ms + processing, causing the mixer to
// under-produce frames relative to the real-time Opus frame rate.
// Closes the output channel on return.
func (m *Mixer) Run(ctx context.Context) {
	defer close(m.out)
	timer := time.NewTimer(mixerFrameDur)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			start := time.Now()
			if err := m.tick(ctx); err != nil {
				slog.Error("mixer: tick error", slog.Any("err", err))
			}
			elapsed := time.Since(start)
			telemetry.MixerTickDuration.Record(ctx, float64(elapsed.Microseconds())/1000)
			// Subtract processing time so the next tick fires closer to 20 ms
			// after the previous one started, not 20 ms after it finished.
			next := mixerFrameDur - elapsed
			if next < time.Millisecond {
				next = time.Millisecond
			}
			timer.Reset(next)
		}
	}
}

func (m *Mixer) tick(ctx context.Context) error {
	paused := m.paused.Load()

	m.mu.Lock()
	m.entriesBuf = m.entriesBuf[:0]
	for _, e := range m.inputs {
		m.entriesBuf = append(m.entriesBuf, e)
	}
	m.mu.Unlock()
	// entriesBuf is safe to read without the lock from here: inputEntry.ch is
	// assigned once in AddInput and never mutated. RemoveInput only deletes the
	// map entry; it does not touch the inputEntry struct itself. Reading a
	// closed or already-drained channel is always safe in Go.

	// Read one frame per input. Only drain to latest when paused (to prevent
	// upstream backpressure) or when the backlog exceeds the threshold
	// (to cap accumulated latency). Under normal jitter (0–2 frames queued)
	// frames are consumed one-per-tick without drops, and the channel buffer
	// absorbs timing misalignment between upstream fanout and the mixer tick.
	m.framesBuf = m.framesBuf[:0]
	for _, e := range m.entriesBuf {
		var latest Frame
		hasFrame := false

		// Non-blocking read of one frame.
		select {
		case f := <-e.ch:
			if len(f.PCM) > 0 {
				latest = f
				hasFrame = true
			}
		default:
		}

		// Drain remaining frames when paused (backpressure relief) or when
		// the active backlog exceeds the threshold (~100 ms of latency).
		if paused || (hasFrame && len(e.ch) > mixerInputDrainThreshold) {
		drain:
			for {
				select {
				case f := <-e.ch:
					if len(f.PCM) > 0 {
						latest = f
						hasFrame = true
					}
				default:
					break drain
				}
			}
		}

		if hasFrame {
			m.framesBuf = append(m.framesBuf, latest)
		}
	}

	// When paused (no non-bot listeners in the destination channel), skip
	// mixing/encoding/output entirely. Inputs were already drained above.
	if paused || len(m.framesBuf) == 0 {
		return nil
	}

	// Record pipeline latency from the oldest input frame (worst-case path).
	now := time.Now()
	oldest := m.framesBuf[0].CreatedAt
	for _, f := range m.framesBuf[1:] {
		if !f.CreatedAt.IsZero() && f.CreatedAt.Before(oldest) {
			oldest = f.CreatedAt
		}
	}
	if !oldest.IsZero() {
		telemetry.MixerPipelineLatency.Record(ctx,
			float64(now.Sub(oldest).Microseconds())/1000)
	}

	// Zero the accumulator in-place instead of allocating a new slice.
	clear(m.mixed)

	// Single active source: forward the original Opus packet directly.
	// No re-encode needed — eliminates 1 encode per tick for the common case.
	// Frame.Opus is already an isolated copy made by the fanout goroutine so no
	// defensive copy is needed here.
	if len(m.framesBuf) == 1 {
		select {
		case m.out <- m.framesBuf[0].Opus:
		default:
			slog.Debug("mixer: output channel full, dropping frame")
		}
		return nil
	}

	// Multiple active sources: accumulate PCM and re-encode (mix-minus).
	// The inner loop is unrolled 4× to help the compiler emit SIMD instructions
	// (SSE2/AVX2/NEON). mixerPCMBuf (1920) is always divisible by 4 so no tail
	// loop is needed.
	for _, f := range m.framesBuf {
		pcm := f.PCM
		for i := 0; i < len(pcm); i += 4 {
			m.mixed[i] += int32(pcm[i])
			m.mixed[i+1] += int32(pcm[i+1])
			m.mixed[i+2] += int32(pcm[i+2])
			m.mixed[i+3] += int32(pcm[i+3])
		}
	}

	// Clamp to int16 range and write into pcm for re-encoding.
	for i, v := range m.mixed {
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		m.pcm[i] = int16(v)
	}

	n, err := m.enc.Encode(m.pcm, m.encodeBuf)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	// Copy the encoded frame so each consumer gets its own slice; encodeBuf is reused next tick.
	out := make([]byte, n)
	copy(out, m.encodeBuf[:n])

	select {
	case m.out <- out:
	default:
		slog.Debug("mixer: output channel full, dropping frame")
	}
	return nil
}
