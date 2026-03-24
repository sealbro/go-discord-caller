package opus

import (
	"log/slog"
	"sync/atomic"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// VoiceReceiver forwards incoming Opus frames into a channel.
type VoiceReceiver struct {
	voice.OpusFrameReceiver
	ch        chan<- []byte
	closed    atomic.Bool
	botID     snowflake.ID
	allowUser func(snowflake.ID) bool // optional; nil means allow all non-bot users
}

func NewVoiceReceiver(ch chan<- []byte, botID snowflake.ID, allowUser func(snowflake.ID) bool) *VoiceReceiver {
	return &VoiceReceiver{
		ch:        ch,
		botID:     botID,
		allowUser: allowUser,
	}
}

func (v *VoiceReceiver) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	if packet == nil {
		return nil
	}

	if v.closed.Load() {
		return nil // receiver is shut down; discard silently
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
}

func (v *VoiceReceiver) Close() {
	v.closed.Store(true)
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
