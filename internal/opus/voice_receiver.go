package opus

import (
	"log/slog"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// VoiceReceiver forwards incoming Opus frames into a channel.
type VoiceReceiver struct {
	voice.OpusFrameReceiver
	ch        chan<- []byte
	done      chan struct{}
	botID     snowflake.ID
	allowUser func(snowflake.ID) bool // optional; nil means allow all non-bot users
}

func NewVoiceReceiver(ch chan<- []byte, botID snowflake.ID, allowUser func(snowflake.ID) bool) *VoiceReceiver {
	return &VoiceReceiver{
		ch:        ch,
		done:      make(chan struct{}),
		botID:     botID,
		allowUser: allowUser,
	}
}

func (v *VoiceReceiver) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	if packet == nil {
		return nil
	}

	// Non-blocking check: if already closed, discard silently.
	select {
	case <-v.done:
		return nil
	default:
	}

	// Ignore frames from our own bot to avoid re-echoing what we send.
	if v.botID != 0 && userID == v.botID {
		return nil
	}

	// Apply optional role/user filter.
	if v.allowUser != nil && !v.allowUser(userID) {
		return nil
	}

	// Copy the opus bytes before sending because the backing array may be reused
	// by the voice library.
	data := make([]byte, len(packet.Opus))
	copy(data, packet.Opus)

	// Try to forward the frame. Selecting on done prevents a send to a
	// channel that the relay goroutine has already stopped draining.
	select {
	case v.ch <- data:
	case <-v.done:
		// receiver was closed between the check above and here; discard safely
	default:
		slog.Info("dropping opus frame: channel full")
	}
	return nil
}

func (v *VoiceReceiver) CleanupUser(userID snowflake.ID) {
	slog.Info("cleanup user", slog.Any("userID", userID))
}

func (v *VoiceReceiver) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
	}
}

// EmptyVoiceReceiver is a no-op OpusFrameReceiver that silently discards all incoming frames.
type EmptyVoiceReceiver struct {
	voice.OpusFrameReceiver
}

func NewEmptyVoiceReceiver() *EmptyVoiceReceiver {
	return &EmptyVoiceReceiver{}
}

func (v *EmptyVoiceReceiver) ReceiveOpusFrame(_ snowflake.ID, _ *voice.Packet) error {
	return nil
}

func (v *EmptyVoiceReceiver) CleanupUser(_ snowflake.ID) {}

func (v *EmptyVoiceReceiver) Close() {}
