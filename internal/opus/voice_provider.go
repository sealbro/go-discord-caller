package opus

import (
	"fmt"

	"github.com/disgoorg/disgo/voice"
)

// VoiceProvider streams Opus frames from a channel into a voice connection.
type VoiceProvider struct {
	voice.OpusFrameProvider
	ch   <-chan []byte
	done chan struct{}
}

func NewVoiceProvider(ch <-chan []byte) *VoiceProvider {
	return &VoiceProvider{
		ch:   ch,
		done: make(chan struct{}),
	}
}

// providerDrainThreshold is the maximum number of queued frames (beyond the
// one just read) before the provider drains to the latest. Normal timing
// jitter between the mixer tick and the disgo sender produces 0–2 queued
// frames; anything above the threshold indicates a stall that accumulated
// latency beyond an acceptable window.
// 5 frames × 20 ms = 100 ms of tolerated jitter before drain kicks in.
const providerDrainThreshold = 5

func (v *VoiceProvider) ProvideOpusFrame() ([]byte, error) {
	select {
	case <-v.done:
		return nil, fmt.Errorf("voice provider is closed")
	case data, ok := <-v.ch:
		if !ok {
			return nil, fmt.Errorf("voice provider channel closed")
		}
		// Only drain to latest when the buffer depth exceeds the threshold,
		// indicating a stall accumulated more than ~100 ms of latency.
		// Under normal jitter (0–2 frames queued) frames play in order
		// without drops, producing smooth audio. Unconditional drain-to-latest
		// drops frames even under mild jitter, causing audible gaps and
		// perceived speed-up of speech (silence between words is removed).
		if len(v.ch) > providerDrainThreshold {
			for {
				select {
				case newer, ok := <-v.ch:
					if !ok {
						return data, nil
					}
					data = newer
				default:
					return data, nil
				}
			}
		}
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
