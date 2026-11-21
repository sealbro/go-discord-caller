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

// disgo can close a receiver from its own goroutine as well as ours, and a
// packet already in flight must not reach the speakers after teardown.
func TestClosedVoiceReceiverForwardsNothing(t *testing.T) {
	handle := NewFanoutHandle()
	// The send to a speaker also selects on the receiver closing, which would
	// still let about half of these through if the up-front check went away.
	const packets = 32
	opusTarget := make(chan []byte, packets)
	handle.Install(FanoutInstall{OpusTargets: []chan<- []byte{opusTarget}})
	receiver := NewVoiceReceiver(0, nil, telemetry.OpusRecorder{}, handle)

	receiver.Close()
	receiver.Close()

	for range packets {
		if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{1, 2, 3}}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := len(opusTarget); got != 0 {
		t.Errorf("closed receiver forwarded %d of %d frames", got, packets)
	}
}

func TestEmptyVoiceReceiverDiscardsEverything(t *testing.T) {
	receiver := NewEmptyVoiceReceiver()
	if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{1, 2, 3}}); err != nil {
		t.Errorf("ReceiveOpusFrame: %v", err)
	}
	receiver.CleanupUser(7)
	receiver.Close()
}
