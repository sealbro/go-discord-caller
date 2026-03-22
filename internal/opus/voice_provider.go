package opus

import (
	"fmt"

	"github.com/disgoorg/disgo/voice"
)

type VoiceProvider struct {
	voice.OpusFrameProvider
	ch     <-chan []byte
	closed bool
}

func NewVoiceProvider(ch <-chan []byte) *VoiceProvider {
	return &VoiceProvider{
		ch: ch,
	}
}

func (v *VoiceProvider) ProvideOpusFrame() ([]byte, error) {
	if v.closed {
		return nil, fmt.Errorf("voice provider is closed")
	}
	// Wait for a frame from the channel. If the provider is closed, return an error.
	data, ok := <-v.ch
	if !ok {
		return nil, fmt.Errorf("voice provider channel closed")
	}
	return data, nil
}

func (v *VoiceProvider) Close() {
	v.closed = true
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
