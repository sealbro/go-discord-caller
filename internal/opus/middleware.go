package opus

import (
	"github.com/disgoorg/disgo/voice"
)

// ProviderMiddleware wraps a voice.OpusFrameProvider to add cross-cutting behavior
// (e.g. recording, transcription). Applied in order in VoiceConnSetup.WithVoiceProvider.
type ProviderMiddleware func(voice.OpusFrameProvider) voice.OpusFrameProvider

// ReceiverMiddleware wraps a voice.OpusFrameReceiver to add cross-cutting behavior.
// Applied in order in VoiceConnSetup.WithVoiceReceiver.
type ReceiverMiddleware func(voice.OpusFrameReceiver) voice.OpusFrameReceiver

// ApplyProviderMiddleware applies each middleware in order, wrapping p.
// Returns p unchanged when mw is empty.
func ApplyProviderMiddleware(p voice.OpusFrameProvider, mw []ProviderMiddleware) voice.OpusFrameProvider {
	for _, m := range mw {
		p = m(p)
	}
	return p
}

// ApplyReceiverMiddleware applies each middleware in order, wrapping r.
// Returns r unchanged when mw is empty.
func ApplyReceiverMiddleware(r voice.OpusFrameReceiver, mw []ReceiverMiddleware) voice.OpusFrameReceiver {
	for _, m := range mw {
		r = m(r)
	}
	return r
}
