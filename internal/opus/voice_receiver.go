package opus

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// recvFrameCap is the pool buffer capacity for raw Opus frames received from Discord.
// Discord voice sends Opus at 48 kHz, 20 ms frames; at typical voice bitrates
// (8–64 kbps) encoded frame size is 20–160 bytes. 256 bytes covers all standard
// bitrates and Opus FEC padding with headroom.
const recvFrameCap = 256

var recvFramePool = &sync.Pool{
	New: func() any {
		return new(make([]byte, recvFrameCap))
	},
}

// getRecvFrame returns a []byte of length n from the receive pool.
// Falls back to a fresh allocation when n exceeds recvFrameCap (rare for voice frames).
func getRecvFrame(n int) []byte {
	if n > recvFrameCap {
		return make([]byte, n)
	}
	return (*recvFramePool.Get().(*[]byte))[:n]
}

// VoiceReceiver forwards incoming Opus frames into a channel.
type VoiceReceiver struct {
	voice.OpusFrameReceiver
	ch        chan<- []byte
	done      chan struct{}
	botID     snowflake.ID
	allowUser func(snowflake.ID) bool // optional; nil means allow all non-bot users
	onDrop    func()                  // called once per frame dropped due to full channel; nil = no-op
	metrics   telemetry.OpusRecorder  // zero-value is safe (no-op)
}

func NewVoiceReceiver(ch chan<- []byte, botID snowflake.ID, allowUser func(snowflake.ID) bool, onDrop func(), metrics telemetry.OpusRecorder) *VoiceReceiver {
	return &VoiceReceiver{
		ch:        ch,
		done:      make(chan struct{}),
		botID:     botID,
		allowUser: allowUser,
		onDrop:    onDrop,
		metrics:   metrics,
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

	start := time.Now()

	// Apply optional role/user filter.
	if v.allowUser != nil && !v.allowUser(userID) {
		return nil
	}

	// Copy the opus bytes before sending because the backing array may be reused
	// by the voice library. Use the pool to avoid a fresh allocation per frame.
	// VoiceProvider.ProvideOpusFrame returns the buffer via PutEncodedFrame after
	// the UDP send completes, so the pool recycles it safely.
	data := getRecvFrame(len(packet.Opus))
	copy(data, packet.Opus)

	// Try to forward the frame. Selecting on done prevents a send to a
	// channel that the relay goroutine has already stopped draining.
	select {
	case v.ch <- data:
	case <-v.done:
	default:
		if v.onDrop != nil {
			v.onDrop()
		}
	}

	v.metrics.RecordReceive(float64(time.Since(start).Microseconds()) / 1000.0)
	return nil
}

func (v *VoiceReceiver) CleanupUser(userID snowflake.ID) {
	slog.Debug("cleanup user", slog.Any("userID", userID))
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
