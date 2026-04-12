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
	mixerOutputBuf  = 15
)

type inputEntry struct {
	ch  <-chan []byte
	dec *hraban.Decoder
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
}

// NewMixer creates a Mixer ready to accept inputs and run.
func NewMixer() (*Mixer, error) {
	enc, err := hraban.NewEncoder(mixerSampleRate, mixerChannels, hraban.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("mixer: new encoder: %w", err)
	}
	return &Mixer{
		inputs: make(map[snowflake.ID]*inputEntry),
		out:    make(chan []byte, mixerOutputBuf),
		enc:    enc,
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
	entries := make([]*inputEntry, 0, len(m.inputs))
	for _, e := range m.inputs {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	mixed := make([]int32, mixerPCMBuf)
	hasAudio := false
	pcm := make([]int16, mixerPCMBuf)

	for _, e := range entries {
		var pkt []byte
		select {
		case pkt = <-e.ch:
		default:
			continue // no frame available; treat as silence
		}
		if len(pkt) == 0 {
			continue
		}
		n, err := e.dec.Decode(pkt, pcm)
		if err != nil {
			slog.Debug("mixer: decode failed", slog.Any("err", err))
			continue
		}
		for i := 0; i < n*mixerChannels; i++ {
			mixed[i] += int32(pcm[i])
		}
		hasAudio = true
	}

	if !hasAudio {
		return nil
	}

	// Clamp to int16 range and write into pcm for re-encoding.
	for i, v := range mixed {
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		pcm[i] = int16(v)
	}

	buf := make([]byte, 4096)
	n, err := m.enc.Encode(pcm, buf)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	select {
	case m.out <- buf[:n]:
	default:
		slog.Debug("mixer: output channel full, dropping frame")
	}
	return nil
}
