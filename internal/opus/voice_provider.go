package opus

import (
	"fmt"

	"github.com/disgoorg/disgo/voice"
)

// VoiceProvider streams Opus frames from a channel into a voice connection.
type VoiceProvider struct {
	voice.OpusFrameProvider
	ch     <-chan []byte
	done   chan struct{}
	onDrop func() // called once per frame silently discarded by the drain loop; nil = no-op
	// prev holds the buffer returned by the last ProvideOpusFrame call.
	// It is recycled via PutEncodedFrame at the start of the next call —
	// by that point disgo has finished sending the packet over UDP and no
	// longer holds a reference to the slice.
	prev []byte
}

func NewVoiceProvider(ch <-chan []byte, onDrop func()) *VoiceProvider {
	return &VoiceProvider{
		ch:     ch,
		done:   make(chan struct{}),
		onDrop: onDrop,
	}
}

// providerDrainThreshold is the maximum number of queued frames (beyond the
// one just read) before the provider drains excess frames. Normal timing
// jitter between the mixer tick and the disgo sender produces 0–2 queued
// frames; anything above the threshold indicates a stall that accumulated
// latency beyond an acceptable window.
// 3 frames × 20 ms = 60 ms of tolerated jitter before drain kicks in.
const providerDrainThreshold = 3

func (v *VoiceProvider) ProvideOpusFrame() ([]byte, error) {
	// Return the previous frame's buffer to the pool before blocking.
	// disgo calls ProvideOpusFrame only after finishing the UDP send for the
	// previous packet, so v.prev is no longer referenced at this point.
	// PutEncodedFrame is a no-op for passthrough slices (wrong cap), so this
	// is safe regardless of whether the frame came from the pool or the receiver.
	PutEncodedFrame(v.prev)
	v.prev = nil

	select {
	case <-v.done:
		return nil, fmt.Errorf("voice provider is closed")
	case data, ok := <-v.ch:
		if !ok {
			return nil, fmt.Errorf("voice provider channel closed")
		}
		// Drain excess frames when the buffer depth exceeds the threshold,
		// but keep the last queued frame in the channel so speech is not
		// cut mid-word. Under normal jitter (0–2 frames queued) frames play
		// in order without drops, producing smooth audio.
		if len(v.ch) > providerDrainThreshold {
			for len(v.ch) > 1 {
				select {
				case newer, ok := <-v.ch:
					if !ok {
						v.prev = data
						return data, nil
					}
					PutEncodedFrame(data) // dropped frame; return to pool
					if v.onDrop != nil {
						v.onDrop()
					}
					data = newer
				default:
					v.prev = data
					return data, nil
				}
			}
		}
		v.prev = data
		return data, nil
	}
}

func (v *VoiceProvider) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
	}
}

// EmptyVoiceProvider is a no-op OpusFrameProvider that never sends audio.
// ProvideOpusFrame blocks until Close is called, at which point it returns an error
// so the audio sender stops cleanly.
type EmptyVoiceProvider struct {
	voice.OpusFrameProvider
	done chan struct{}
}

func NewEmptyVoiceProvider() *EmptyVoiceProvider {
	return &EmptyVoiceProvider{
		done: make(chan struct{}),
	}
}

func (v *EmptyVoiceProvider) ProvideOpusFrame() ([]byte, error) {
	<-v.done
	return nil, fmt.Errorf("empty voice provider closed")
}

func (v *EmptyVoiceProvider) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
	}
}
