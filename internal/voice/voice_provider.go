package voice

import (
	"fmt"

	"github.com/disgoorg/disgo/voice"
)

type VoiceProvider struct {
	voice.OpusFrameProvider
	ch     chan []byte
	closed bool
}

func NewVoiceProvider(ch chan []byte) *VoiceProvider {
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
