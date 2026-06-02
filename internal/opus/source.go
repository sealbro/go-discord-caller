package opus

import "sync"

// audioSourceCap is the ring-buffer capacity per mixer input.
// 3 frames × 20 ms = 60 ms max age before the oldest frame is overwritten on
// overflow. Compared to the previous 5-frame buffered channel with a 3-frame
// drain threshold (~100 ms worst-case queue depth), this cuts worst-case mixer
// input latency to 40 ms while absorbing typical 1–2 frame jitter.
const audioSourceCap = 3

// AudioSource is the pull interface consumed by the Mixer tick.
// Producers (fanout dispatch, relay bridge) call Feed on the concrete
// *SourceBuffer; the mixer calls Pull/Drain via this interface.
type AudioSource interface {
	// Pull removes and returns the oldest buffered frame.
	// Returns (Frame{}, false) on underrun (no frame available this tick).
	Pull() (Frame, bool)
	// Drain discards all buffered frames and returns their pool buffers.
	// Called by the mixer when paused to prevent PCM/Opus buffer leaks.
	Drain()
	// Close is a lifecycle no-op; sources are detached via Mixer.RemoveInput.
	Close()
}

// SourceBuffer is a fixed-capacity lock-protected ring buffer that implements
// AudioSource. Both fanout dispatch (fed inline by VoiceReceiver) and the relay
// bridge goroutine (fed by the relay decoder) use this type.
//
// Overflow policy: when the ring is full, Feed silently discards the oldest
// frame (returning its pool buffers) and records a drop. This keeps the consumer
// always near the live edge — the same goal the previous drain-threshold logic
// achieved gradually, but applied instantly at the producer.
type SourceBuffer struct {
	mu   sync.Mutex
	buf  [audioSourceCap]Frame
	head int // index of oldest entry
	n    int // number of valid entries
	drop func()
}

// NewSourceBuffer creates a SourceBuffer. drop is called each time overflow
// forces the oldest frame out. Pass nil to silence the counter.
func NewSourceBuffer(drop func()) *SourceBuffer {
	return &SourceBuffer{drop: drop}
}

// Feed inserts f into the ring. If the ring is full the oldest frame is evicted
// (its pool buffers returned) and drop is invoked before inserting the new frame.
// Safe to call concurrently with Pull/Drain.
func (s *SourceBuffer) Feed(f Frame) {
	s.mu.Lock()
	if s.n == audioSourceCap {
		old := s.buf[s.head]
		if old.PCM != nil {
			PutPCM(old.PCM)
		}
		if old.Opus != nil {
			PutEncodedFrame(old.Opus)
		}
		s.head = (s.head + 1) % audioSourceCap
		s.n--
		if s.drop != nil {
			s.drop()
		}
	}
	s.buf[(s.head+s.n)%audioSourceCap] = f
	s.n++
	s.mu.Unlock()
}

// Len returns the number of frames currently buffered. Intended for
// diagnostic reads (e.g. the mixer's paused-drain detection); racey by
// nature — callers must not depend on the value for correctness.
func (s *SourceBuffer) Len() int {
	s.mu.Lock()
	n := s.n
	s.mu.Unlock()
	return n
}

// Pull removes and returns the oldest frame. Returns (Frame{}, false) on underrun.
func (s *SourceBuffer) Pull() (Frame, bool) {
	s.mu.Lock()
	if s.n == 0 {
		s.mu.Unlock()
		return Frame{}, false
	}
	f := s.buf[s.head]
	s.head = (s.head + 1) % audioSourceCap
	s.n--
	s.mu.Unlock()
	return f, true
}

// Drain discards all buffered frames and returns their pool buffers.
func (s *SourceBuffer) Drain() {
	s.mu.Lock()
	for s.n > 0 {
		f := s.buf[s.head]
		s.head = (s.head + 1) % audioSourceCap
		s.n--
		if f.PCM != nil {
			PutPCM(f.PCM)
		}
		if f.Opus != nil {
			PutEncodedFrame(f.Opus)
		}
	}
	s.mu.Unlock()
}

// Close is a no-op; lifecycle is managed via Mixer.RemoveInput.
func (s *SourceBuffer) Close() {}
