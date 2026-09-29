package opus

import (
	"testing"

	"github.com/disgoorg/disgo/voice"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// A DAVE packet the decrypt layer drops arrives here with an empty payload, and
// Discord's own silence frame is three bytes — so a zero-length payload is never
// audio. Forwarding it sends empty packets on to every speaker and fails the
// decoder once per frame.
func TestEmptyOpusFrameIsNotForwarded(t *testing.T) {
	handle := NewFanoutHandle()
	opusTarget := make(chan []byte, 1)
	callbacks := 0
	handle.Install(FanoutInstall{
		OpusTargets: []chan<- []byte{opusTarget},
		OpusCallback: func(b []byte) {
			callbacks++
			PutEncodedFrame(b)
		},
	})

	receiver := NewVoiceReceiver(0, nil, telemetry.OpusRecorder{}, handle)

	if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: nil}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case frame := <-opusTarget:
		t.Errorf("forwarded a %d-byte frame to a speaker; the packet carried no audio", len(frame))
	default:
	}
	if callbacks != 0 {
		t.Errorf("relay callback ran %d times for a packet with no audio", callbacks)
	}
}
