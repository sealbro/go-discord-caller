package opus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	hraban "github.com/hraban/opus"

	"github.com/disgoorg/snowflake/v2"
)

const (
	mixerSampleRate = 48000
	mixerChannels   = 2
	mixerFrameSize  = 960 // samples per channel for 20 ms at 48 kHz
	mixerPCMBuf     = mixerFrameSize * mixerChannels
	mixerFrameDur   = 20 * time.Millisecond
	// mixerOutputBuf is the output channel buffer depth (30 frames × 20 ms = 600 ms).
	// Frames are dropped silently when the consumer falls more than 600 ms behind.
	// Increase this if guest guilds experience frequent audio gaps under load.
	mixerOutputBuf = 30
	// silenceResetThreshold is the number of consecutive silent ticks (~500 ms)
	// after which PLC is skipped and the decoder is considered cold. When a real
	// frame arrives the silent counter resets and normal decoding resumes.
	silenceResetThreshold = 25
)

type inputEntry struct {
	ch          <-chan []byte
	dec         *hraban.Decoder
	silentTicks int
}

// Mixer receives Opus frames from multiple named input channels, mixes their PCM,
// and outputs a single re-encoded Opus stream on Output().
// Call Run to start the 20 ms tick loop; it blocks until ctx is cancelled and
// closes the output channel on return.
type Mixer struct {
	mu     sync.Mutex
	inputs map[snowflake.ID]*inputEntry

	out chan []byte
	enc *hraban.Encoder

	// Pre-allocated scratch buffers reused on every tick.
	// Only accessed from the single Run goroutine — no synchronisation needed.
	entriesBuf []*inputEntry
	mixed      []int32
	pcm        []int16
	encodeBuf  []byte
}

// mixerComplexity is the Opus encoder complexity (0–10).
// Default is 9 (max quality); 3 gives ~60% CPU reduction vs default with
// acceptable voice quality for relay use cases.
const mixerComplexity = 3

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
	return &Mixer{
		inputs:    make(map[snowflake.ID]*inputEntry),
		out:       make(chan []byte, mixerOutputBuf),
		enc:       enc,
		mixed:     make([]int32, mixerPCMBuf),
		pcm:       make([]int16, mixerPCMBuf),
		encodeBuf: make([]byte, 4096),
	}, nil
}

// AddInput registers a new audio source identified by id.
func (m *Mixer) AddInput(id snowflake.ID, ch <-chan []byte) error {
	dec, err := hraban.NewDecoder(mixerSampleRate, mixerChannels)
	if err != nil {
		return fmt.Errorf("mixer: new decoder for %s: %w", id, err)
	}
	m.mu.Lock()
	m.inputs[id] = &inputEntry{ch: ch, dec: dec}
	m.mu.Unlock()
	return nil
}

// RemoveInput unregisters the audio source identified by id.
func (m *Mixer) RemoveInput(id snowflake.ID) {
	m.mu.Lock()
	delete(m.inputs, id)
	m.mu.Unlock()
}

// Output returns the channel carrying mixed Opus frames.
func (m *Mixer) Output() <-chan []byte {
	return m.out
}

// Run ticks every 20 ms, mixing all pending frames, until ctx is cancelled.
// Closes the output channel on return.
func (m *Mixer) Run(ctx context.Context) {
	ticker := time.NewTicker(mixerFrameDur)
	defer func() {
		ticker.Stop()
		close(m.out)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.tick(); err != nil {
				slog.Error("mixer: tick error", slog.Any("err", err))
			}
		}
	}
}

func (m *Mixer) tick() error {
	m.mu.Lock()
	m.entriesBuf = m.entriesBuf[:0]
	for _, e := range m.inputs {
		m.entriesBuf = append(m.entriesBuf, e)
	}
	m.mu.Unlock()

	// Zero the accumulator in-place instead of allocating a new slice.
	clear(m.mixed)
	hasAudio := false

	for _, e := range m.entriesBuf {
		var pkt []byte
		select {
		case pkt = <-e.ch:
			e.silentTicks = 0
			if len(pkt) > 0 {
				hasAudio = true
			}
		default:
			// No frame available. Skip PLC entirely once the input has been
			// silent long enough — avoids expensive Opus PLC decodes during
			// sustained silence.
			e.silentTicks++
			if e.silentTicks > silenceResetThreshold {
				continue
			}
			// pkt stays nil → decoder applies PLC for short gaps
		}
		n, err := e.dec.Decode(pkt, m.pcm)
		if err != nil {
			slog.Debug("mixer: decode failed", slog.Any("err", err))
			continue
		}
		for i := 0; i < n*mixerChannels; i++ {
			m.mixed[i] += int32(m.pcm[i])
		}
	}

	if !hasAudio {
		return nil
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
