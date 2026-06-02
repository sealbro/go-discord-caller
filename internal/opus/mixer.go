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
	mixerBitrate    = 48000 // bits per second sent to Opus encoder
)

// mixerComplexity is the Opus encoder complexity (0–10).
// 5 is a good middle ground: significantly better quality than 3 for mixed
// multi-source paths with modest additional CPU (~15% over complexity 3).
const mixerComplexity = 5

// encodedFrameCap is the pool buffer capacity for re-encoded Opus output frames,
// calculated as 4× the nominal CBR frame size to absorb VBR overshoot and FEC padding.
// Nominal: mixerBitrate (bps) × frame duration (ms) / 1000 / 8 bytes
//
//	= 48000 × 20 / 1000 / 8 = 120 bytes  →  pool cap = 480 bytes.
//
// frame duration in ms = mixerFrameSize samples / (mixerSampleRate / 1000) = 960 / 48 = 20.
const encodedFrameCap = mixerBitrate * (mixerFrameSize / (mixerSampleRate / 1000)) / 1000 / 8 * 4

// encodedBuf is a fixed-size array backing encoded-frame pool entries (single allocation on miss).
type encodedBuf [encodedFrameCap]byte

// pcmBuf is a fixed-size array backing PCM pool entries (single allocation on miss).
type pcmBuf [mixerPCMBuf]int16

// pcmPool recycles PCM buffers ([]int16 of length MixerPCMBuf = 1920) used by
// fanout goroutines and returned by mixer tick after consumption.
var pcmPool = &sync.Pool{
	New: func() any { return new(pcmBuf) },
}

// GetPCM returns a []int16 of length MixerPCMBuf from the pool.
// The caller owns the slice until it is returned via PutPCM.
func GetPCM() []int16 { return pcmPool.Get().(*pcmBuf)[:] }

// PutPCM returns a PCM buffer to the pool for reuse.
// The caller must not access the slice after this call.
func PutPCM(s []int16) { pcmPool.Put((*pcmBuf)(s[:mixerPCMBuf])) }

var encodedFramePool = &sync.Pool{
	New: func() any { return new(encodedBuf) },
}

// getEncodedFrame returns a []byte of length n from the pool.
// Falls back to a fresh allocation when n exceeds encodedFrameCap (rare).
func getEncodedFrame(n int) []byte {
	if n > encodedFrameCap {
		return make([]byte, n)
	}
	return encodedFramePool.Get().(*encodedBuf)[:n]
}

// PutEncodedFrame returns a buffer to the appropriate pool based on its capacity.
// Buffers with cap == encodedFrameCap go to encodedFramePool (re-encoded mixer output).
// Buffers with cap == recvFrameCap go to recvFramePool (raw Opus received from Discord).
// All other capacities are silently dropped; the GC reclaims them.
// The caller must not access the slice after this call.
func PutEncodedFrame(b []byte) {
	switch cap(b) {
	case encodedFrameCap:
		encodedFramePool.Put((*encodedBuf)(b[:encodedFrameCap]))
	case recvFrameCap:
		recvFramePool.Put((*recvBuf)(b[:recvFrameCap]))
	}
}

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

// inputEntry holds a single mixer input backed by an AudioSource.
type inputEntry struct {
	src AudioSource
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
	mu             sync.Mutex
	inputs         map[snowflake.ID]*inputEntry
	paused         atomic.Bool
	lastActivityAt atomic.Int64  // UnixNano of last tick that consumed at least one frame
	pausedDrops    atomic.Uint64 // diagnostic: frames discarded by tick because the mixer was paused
	metrics        telemetry.OpusRecorder

	// sink is the destination callback for produced frames. It is invoked
	// synchronously from tick and must not block (multicast to destination
	// channels with non-blocking sends). Set once before Run via SetSink;
	// reads from tick are safe via happens-before through goroutine start.
	sink func(pkt []byte)

	enc *hraban.Encoder

	// Pre-allocated scratch buffers reused on every tick.
	// Only accessed from the single Run goroutine — no synchronisation needed.
	entriesBuf []*inputEntry
	framesBuf  []Frame // active frames collected each tick
	mixed      []int32
	pcm        []int16
	encodeBuf  []byte
}

// NewMixer creates a Mixer ready to accept inputs and run.
// metrics is a pre-baked recorder (guild_id already embedded); obtain one via
// OpusMetrics.For(guildID).
func NewMixer(metrics telemetry.OpusRecorder) (*Mixer, error) {
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
	if err := enc.SetBitrate(mixerBitrate); err != nil {
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
	mx := &Mixer{
		inputs:    make(map[snowflake.ID]*inputEntry),
		enc:       enc,
		metrics:   metrics,
		mixed:     make([]int32, mixerPCMBuf),
		pcm:       make([]int16, mixerPCMBuf),
		encodeBuf: make([]byte, 4096),
		framesBuf: make([]Frame, 0, 8),
	}
	mx.lastActivityAt.Store(time.Now().UnixNano())
	return mx, nil
}

// SetSink registers the destination callback invoked once per produced frame.
// Must be called before Run. The callback runs synchronously inside tick and
// must not block — fan out to destination channels with non-blocking sends.
func (m *Mixer) SetSink(sink func(pkt []byte)) {
	m.sink = sink
}

// AddInput registers an AudioSource identified by id.
func (m *Mixer) AddInput(id snowflake.ID, src AudioSource) error {
	m.mu.Lock()
	m.inputs[id] = &inputEntry{src: src}
	m.mu.Unlock()
	return nil
}

// RemoveInput unregisters the audio source identified by id.
func (m *Mixer) RemoveInput(id snowflake.ID) {
	m.mu.Lock()
	delete(m.inputs, id)
	m.mu.Unlock()
}

// InputIDs returns the IDs of all currently registered inputs in unspecified order.
// Intended for tests and observability — the result is a snapshot taken under
// the mixer's lock and not safe to mutate.
func (m *Mixer) InputIDs() []snowflake.ID {
	m.mu.Lock()
	ids := make([]snowflake.ID, 0, len(m.inputs))
	for id := range m.inputs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	return ids
}

// SetPaused controls whether the mixer is paused. While paused, tick drains
// input channels (preventing upstream backpressure) but skips mixing, encoding,
// and output. Use this to suspend mixers whose destination channel has no
// non-bot listeners.
func (m *Mixer) SetPaused(p bool) {
	m.paused.Store(p)
}

// PausedDrops returns the cumulative number of input frames discarded by
// tick because the mixer was paused at tick time. Useful for tests and
// diagnostic dashboards — non-zero values indicate upstream packets are
// arriving but being silently dropped at the mixer boundary.
func (m *Mixer) PausedDrops() uint64 {
	return m.pausedDrops.Load()
}

// Paused reports whether the mixer is currently paused.
func (m *Mixer) Paused() bool {
	return m.paused.Load()
}

// IdleFor returns the duration since the last tick that consumed at least one
// input frame. Returns 0 if no frames have ever been consumed (the mixer was
// just created and the idle countdown starts from NewMixer).
func (m *Mixer) IdleFor() time.Duration {
	t := m.lastActivityAt.Load()
	if t == 0 {
		return 0
	}
	return time.Since(time.Unix(0, t))
}

// Run fires a 20 ms mix tick, waits for it to complete, then schedules the next
// one. Using time.Timer (reset after each tick) instead of time.Ticker prevents
// stale-frame buildup: if a tick takes longer than 20 ms under load, the next
// tick is deferred rather than queued immediately from a backlog.
// The timer accounts for tick processing time to prevent systematic drift:
// without correction each period is 20 ms + processing, causing the mixer to
// under-produce frames relative to the real-time Opus frame rate.
// The first tick fires immediately so any frames already queued at session
// start are processed without a one-off 20 ms wait.
func (m *Mixer) Run(ctx context.Context) {
	timer := time.NewTimer(time.Microsecond)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			start := time.Now()
			if err := m.tick(); err != nil {
				slog.Error("mixer: tick error", slog.Any("err", err))
			}
			elapsed := time.Since(start)
			m.metrics.RecordTick(float64(elapsed.Microseconds()) / 1000)
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

func (m *Mixer) tick() error {
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

	// Pull one frame per input. When paused, drain all buffered frames instead
	// to prevent PCM/Opus buffer accumulation (SourceBuffer holds pooled memory).
	// Overflow/jitter handling is now inside SourceBuffer.Feed (drops oldest on
	// overflow), so no drain-threshold bleed-off logic is needed here.
	m.framesBuf = m.framesBuf[:0]
	for _, e := range m.entriesBuf {
		if paused {
			// Count what we're about to discard so diagnostic readers (tests,
			// PausedDrops accessor) can detect "upstream is feeding, downstream
			// is silently throwing it away" without enabling debug logging.
			if sb, ok := e.src.(*SourceBuffer); ok {
				if n := sb.Len(); n > 0 {
					m.pausedDrops.Add(uint64(n))
				}
			}
			e.src.Drain()
			continue
		}
		f, ok := e.src.Pull()
		if ok && len(f.PCM) > 0 {
			m.framesBuf = append(m.framesBuf, f)
		}
	}

	if len(m.framesBuf) > 0 {
		m.lastActivityAt.Store(time.Now().UnixNano())
	}

	// When paused (no non-bot listeners in the destination channel), skip
	// mixing/encoding/output entirely. Inputs were already drained above.
	if paused || len(m.framesBuf) == 0 {
		for _, f := range m.framesBuf {
			PutPCM(f.PCM)
		}
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
		m.metrics.RecordPipelineLatency(float64(now.Sub(oldest).Microseconds()) / 1000)
	}

	// Zero the accumulator in-place instead of allocating a new slice.
	clear(m.mixed)

	// Single active source: forward the original Opus packet directly.
	// No re-encode needed — eliminates 1 encode per tick for the common case.
	// Frame.Opus is already an isolated copy made by the fanout goroutine so no
	// defensive copy is needed here.
	if len(m.framesBuf) == 1 {
		PutPCM(m.framesBuf[0].PCM)
		if m.sink != nil {
			m.sink(m.framesBuf[0].Opus)
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
	for _, f := range m.framesBuf {
		PutPCM(f.PCM)
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

	// Copy the encoded frame into a pooled buffer; encodeBuf is reused next tick.
	// VoiceProvider returns this buffer via PutEncodedFrame before blocking for the next frame.
	out := getEncodedFrame(n)
	copy(out, m.encodeBuf[:n])

	if m.sink != nil {
		m.sink(out)
	} else {
		PutEncodedFrame(out) // no sink registered — recycle directly
	}
	return nil
}
