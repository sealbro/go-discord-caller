package opus

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// recvFrameCap is the pool buffer capacity for raw Opus frames received from Discord.
// Discord voice sends Opus at 48 kHz, 20 ms frames; at typical voice bitrates
// (8–64 kbps) encoded frame size is 20–160 bytes. 256 bytes covers all standard
// bitrates and Opus FEC padding with headroom.
const recvFrameCap = 256

// recvBuf is a fixed-size array backing receive pool entries.
// Using *[N]byte instead of *[]byte means pool.New performs one allocation
// (the array itself) rather than two (backing array + slice header pointer).
type recvBuf [recvFrameCap]byte

var recvFramePool = &sync.Pool{
	New: func() any { return new(recvBuf) },
}

// getRecvFrame returns a []byte of length n from the receive pool.
// Falls back to a fresh allocation when n exceeds recvFrameCap (rare for voice frames).
func getRecvFrame(n int) []byte {
	if n > recvFrameCap {
		return make([]byte, n)
	}
	return recvFramePool.Get().(*recvBuf)[:n]
}

// CopyOpusFrame allocates a pool-backed copy of src.
// The caller owns the returned slice and must eventually pass it to PutEncodedFrame.
// Use this when one Opus packet must be sent to multiple independent consumers
// (e.g. multiple VoiceProviders) to avoid returning the same backing array to
// the pool more than once.
func CopyOpusFrame(src []byte) []byte {
	dst := getRecvFrame(len(src))
	copy(dst, src)
	return dst
}

// FanoutInstall describes the destinations a VoiceReceiver should fan each
// incoming Opus packet to once decoding is enabled. Built once per source by
// the wiring code (e.g. wireFanout) and applied via FanoutHandle.Install.
type FanoutInstall struct {
	// OpusTargets receive raw (pooled) Opus bytes. Used by topologies that
	// forward packets without re-encoding (e.g. owner→speaker direct path
	// in star topology). Each target gets its own pooled copy.
	OpusTargets []chan<- []byte
	// SourceTargets receive decoded Frames via SourceBuffer.Feed. Each target
	// gets its own PCM and Opus copy so the mixer's single-source passthrough
	// optimisation can safely forward Opus bytes without re-encoding.
	// Overflow is handled inside SourceBuffer (oldest frame dropped); no
	// select/default or separate DropFrame counter is needed here.
	SourceTargets []*SourceBuffer
	// OnClose is called once when the FanoutHandle is closed (session-end
	// teardown). Use it to detach mixer inputs (RemoveInput) and drain any
	// remaining SourceBuffer frames. NOT called on reconnect-time receiver close.
	OnClose func()
	// DropOpus is invoked each time an OpusTargets send is dropped because
	// the destination channel is full. Optional.
	DropOpus func()
}

// FanoutHandle is the session-stable dispatch state shared across reconnects
// of the same logical voice source. Construct one per source via NewFanoutHandle,
// pass it to NewVoiceReceiver, and after the topology has wired all targets
// call Install once. On reconnect the new VoiceReceiver passes the same handle
// and resumes dispatching to the same targets — wiring code is unaware.
//
// Lifecycle: VoiceReceiver.Close stops only the receiver. The handle is
// detached separately via Close at session end (after all reconnect activity
// has stopped) so that the closeOnce on OnClose does not fire prematurely
// when the OLD receiver is torn down on reconnect.
type FanoutHandle struct {
	state     atomic.Pointer[fanoutDispatch]
	closeOnce sync.Once
}

// NewFanoutHandle creates an empty handle. Frames received before Install fall
// through to the receiver's legacy chan path (or are silently dropped if the
// receiver was constructed with a nil ch), so the brief wiring window between
// SetOpusFrameReceiver and Install does not lose audio for topologies that
// drain the chan, and never blocks the receiver for those that don't.
func NewFanoutHandle() *FanoutHandle { return &FanoutHandle{} }

// fanoutDispatch is the immutable bundle stored in the handle's atomic
// pointer once Install runs.
type fanoutDispatch struct {
	opusTargets   []chan<- []byte
	sourceTargets []*SourceBuffer
	onClose       func()
	dropOpus      func()
}

// Install activates fanout mode for this handle. Idempotent in the sense that
// the latest Install wins; typically called once per session.
func (h *FanoutHandle) Install(opts FanoutInstall) {
	h.state.Store(&fanoutDispatch{
		opusTargets:   opts.OpusTargets,
		sourceTargets: opts.SourceTargets,
		onClose:       opts.OnClose,
		dropOpus:      opts.DropOpus,
	})
}

// Close runs the install-time OnClose hook exactly once. Call from session-end
// teardown — NOT from VoiceReceiver.Close, since that runs on every reconnect.
func (h *FanoutHandle) Close() {
	h.closeOnce.Do(func() {
		s := h.state.Load()
		if s != nil && s.onClose != nil {
			s.onClose()
		}
	})
}

// VoiceReceiver forwards incoming Opus frames into a channel (legacy mode) or
// directly fans them out to mixer inputs after Opus decode (fanout mode).
// Mode is selected at construction by the presence of a FanoutHandle.
type VoiceReceiver struct {
	voice.OpusFrameReceiver
	ch        chan<- []byte // legacy bytes mode; may be nil when fanout-only
	fanout    *FanoutHandle // when non-nil, ReceiveOpusFrame decodes + dispatches
	done      chan struct{}
	botID     snowflake.ID
	allowUser func(snowflake.ID) bool // optional; nil means allow all non-bot users
	metrics   telemetry.OpusRecorder  // zero-value is safe (no-op); drop callback set via OpusRecorder.WithDrop

	// Per-receiver decoder + scratch buffer for fanout-mode dispatch.
	// Lazy-init on first FrameTargets dispatch. Single-producer (disgo
	// serialises ReceiveOpusFrame per receiver) so no synchronisation needed.
	decoder *hraban.Decoder
	scratch []int16
}

// NewVoiceReceiver constructs a VoiceReceiver. Pass a non-nil fanout handle to
// activate inline decode + fanout (the wiring code must call handle.Install
// after building the topology). Pass nil to use legacy chan-based forwarding.
func NewVoiceReceiver(ch chan<- []byte, botID snowflake.ID, allowUser func(snowflake.ID) bool, metrics telemetry.OpusRecorder, fanout *FanoutHandle) *VoiceReceiver {
	return &VoiceReceiver{
		ch:        ch,
		fanout:    fanout,
		done:      make(chan struct{}),
		botID:     botID,
		allowUser: allowUser,
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

	// Fanout mode: decode + multicast inline. Removes a buffered chan hop and
	// the dedicated fanout goroutine that previously sat between us and the
	// mixers (~2–10 ms scheduler wake-up cost eliminated per frame).
	// Topologies that never install (RaidModeOneCaller direct passthrough)
	// fall through to the legacy chan path below.
	if v.fanout != nil {
		if state := v.fanout.state.Load(); state != nil {
			v.dispatchFanout(state, packet.Opus)
			v.recordReceiveLatency(start)
			return nil
		}
	}

	// Legacy chan-bytes mode (RaidModeOneCaller / direct passthrough, or the
	// brief pre-install window for fanout pipelines).
	// Copy the opus bytes before sending because the backing array may be reused
	// by the voice library. Use the pool to avoid a fresh allocation per frame.
	// VoiceProvider.ProvideOpusFrame returns the buffer via PutEncodedFrame after
	// the UDP send completes, so the pool recycles it safely.
	if v.ch == nil {
		v.recordReceiveLatency(start)
		return nil
	}
	data := getRecvFrame(len(packet.Opus))
	copy(data, packet.Opus)

	// Try to forward the frame. Selecting on done prevents a send to a
	// channel that the relay goroutine has already stopped draining.
	select {
	case v.ch <- data:
	case <-v.done:
	default:
		v.metrics.RecordDrop()
	}

	v.recordReceiveLatency(start)
	return nil
}

// dispatchFanout decodes once and multicasts the packet to all configured
// targets. Allocates exactly one shared pooled Opus buffer (referenced by all
// frame and opus targets) and one PCM buffer per frame target.
func (v *VoiceReceiver) dispatchFanout(state *fanoutDispatch, opusBytes []byte) {
	// Lazy-init the decoder on the first frame-target dispatch. A receiver
	// with only OpusTargets never allocates one.
	if v.decoder == nil && len(state.sourceTargets) > 0 {
		dec, err := hraban.NewDecoder(MixerSampleRate, MixerChannels)
		if err != nil {
			slog.Error("voice receiver: failed to init decoder", slog.Any("err", err))
			return
		}
		v.decoder = dec
		v.scratch = make([]int16, MixerPCMBuf)
	}

	// Each OpusTarget (VoiceProvider) independently returns its buffer via
	// PutEncodedFrame after the UDP send completes. Sharing one buffer across
	// multiple targets would cause double-returns to the pool and data races.
	for _, out := range state.opusTargets {
		buf := getRecvFrame(len(opusBytes))
		copy(buf, opusBytes)
		select {
		case out <- buf:
		case <-v.done:
			PutEncodedFrame(buf)
			return
		default:
			PutEncodedFrame(buf)
			if state.dropOpus != nil {
				state.dropOpus()
			}
		}
	}

	if len(state.sourceTargets) == 0 || v.decoder == nil {
		return
	}

	n, err := v.decoder.Decode(opusBytes, v.scratch)
	if err != nil {
		slog.Debug("voice receiver: decode failed", slog.Any("err", err))
		return
	}
	now := time.Now()
	// Each sourceTarget gets its own PCM and Opus copy. SourceBuffer.Feed
	// handles overflow internally (drops oldest + calls its drop func).
	// No select/done check needed: Feed is synchronous and non-blocking.
	for _, t := range state.sourceTargets {
		pcm := GetPCM()[:n*MixerChannels]
		copy(pcm, v.scratch[:n*MixerChannels])
		opusCopy := getRecvFrame(len(opusBytes))
		copy(opusCopy, opusBytes)
		t.Feed(Frame{PCM: pcm, Opus: opusCopy, CreatedAt: now})
	}
}

func (v *VoiceReceiver) recordReceiveLatency(start time.Time) {
	v.metrics.RecordReceive(float64(time.Since(start).Microseconds()) / 1000.0)
}

func (v *VoiceReceiver) CleanupUser(userID snowflake.ID) {
	slog.Debug("cleanup user", slog.Any("userID", userID))
}

// Close stops this receiver. Does NOT close the FanoutHandle — that lives
// across reconnects and is closed separately at session-end teardown.
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
