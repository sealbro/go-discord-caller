package voice

import (
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

type VoiceReceiver struct {
	voice.OpusFrameReceiver
	ch     chan<- []byte
	closed bool
	botID  snowflake.ID
}

func NewVoiceReceiver(ch chan<- []byte, botID snowflake.ID) *VoiceReceiver {
	return &VoiceReceiver{
		ch:    ch,
		botID: botID,
	}
}

func (v *VoiceReceiver) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	if packet == nil {
		return nil
	}

	if v.closed {
		return fmt.Errorf("voice receiver is closed for user %d", userID)
	}

	// Ignore frames from our own bot to avoid re-echoing what we send.
	if v.botID != 0 && userID == v.botID {
		return nil
	}

	// Copy the opus bytes before sending because the backing array may be reused
	// by the voice library.
	data := make([]byte, len(packet.Opus))
	copy(data, packet.Opus)

	// Try to send the frame into the channel. If the channel is full, drop the frame
	// to avoid blocking the receiver goroutine.
	select {
	case v.ch <- data:
	default:
		slog.Info("dropping opus frame: channel full")
	}
	return nil
}

func (v *VoiceReceiver) CleanupUser(userID snowflake.ID) {
	slog.Info("cleanup user", slog.Any("userID", userID))
	return
}

func (v *VoiceReceiver) Close() {
	v.closed = true
}
