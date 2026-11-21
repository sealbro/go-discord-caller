package opus

import "sync"

// audioSourceCap is the starting ring-buffer capacity per mixer input:
// 3 frames × 20 ms = 60 ms, enough for the 1–2 frame jitter of a nearby voice
// server. An input whose bursts overflow it grows one frame per eviction, up
// to maxAudioSourceCap, and shrinks back one frame per sourceShrinkWindow fed
// frames that never filled it. A caller on a distant voice server delivers
// 4-frame bursts all session long; at a fixed 3 frames each burst evicted one,
// losing ~10% of the audio, while a fixed larger ring would add delay to
// every input that does not need it.
const (
	audioSourceCap     = 3
	maxAudioSourceCap  = 6
	sourceShrinkWindow = 500 // ~10 s of speech
)

// AudioSource is the pull interface consumed by the Mixer tick.
// Producers (fanout dispatch, relay bridge) call Feed on the concrete
// *SourceBuffer; the mixer calls Pull/Drain/Len via this interface. Sources are
// detached via Mixer.RemoveInput.
type AudioSource interface {
	// Pull removes and returns the oldest buffered frame.
	// Returns (Frame{}, false) on underrun (no frame available this tick).
	Pull() (Frame, bool)
	// Drain discards all buffered frames and returns their pool buffers.
	// Called by the mixer when paused to prevent PCM/Opus buffer leaks.
	Drain()
	// Len returns the number of frames currently buffered. Used by the paused
	// branch of Mixer.tick to count silently-dropped frames; racey by nature.
	Len() int
}

// SourceBuffer is a bounded lock-protected ring buffer that implements
// AudioSource. Both fanout dispatch (fed inline by VoiceReceiver) and the relay
// bridge goroutine (fed by the relay decoder) use this type.
//
// Overflow policy: when the ring is full, Feed silently discards the oldest
// frame (returning its pool buffers) and records a drop. This keeps the consumer
// always near the live edge — the same goal the previous drain-threshold logic
// achieved gradually, but applied instantly at the producer.
type SourceBuffer struct {
	mu       sync.Mutex
	buf      [maxAudioSourceCap]Frame
	head     int // index of oldest entry
	n        int // number of valid entries
	capacity int // current, audioSourceCap..maxAudioSourceCap
	// peak is the highest occupancy since the last shrink check, fed counts
	// the frames fed since then.
	peak int
	fed  int
	drop func()
	// mixer is the mixer this buffer feeds, set by Mixer.AddInput. While that
	// mixer is paused Feed discards instead of buffering, so the mixer does
	// not have to tick at the frame rate just to throw the frames away.
	mixer *Mixer
}

// NewSourceBuffer creates a SourceBuffer. drop is called each time overflow
// forces the oldest frame out. Pass nil to silence the counter.
func NewSourceBuffer(drop func()) *SourceBuffer {
	return &SourceBuffer{capacity: audioSourceCap, drop: drop}
}

// Feed inserts f into the ring and returns how many frames were already
// buffered ahead of it and whether one was evicted to make room.
// If the ring is full the oldest frame is evicted
// (its pool buffers returned) and drop is invoked AFTER releasing the mutex
// so a slow drop callback doesn't widen the buffer's critical section.
// Safe to call concurrently with Pull/Drain.
//
// A frame fed while the buffer's mixer is paused is discarded and counted as
// a paused drop rather than buffered; that is not an overflow, so drop is not
// invoked for it.
func (s *SourceBuffer) Feed(f Frame) (ahead int, evicted bool) {
	s.mu.Lock()
	if m := s.mixer; m != nil && m.paused.Load() {
		s.mu.Unlock()
		if f.PCM != nil {
			PutPCM(f.PCM)
		}
		if f.Opus != nil {
			PutEncodedFrame(f.Opus)
		}
		m.discardPaused()
		return 0, false
	}
	ahead = s.n
	if s.n == s.capacity {
		old := s.buf[s.head]
		if old.PCM != nil {
			PutPCM(old.PCM)
		}
		if old.Opus != nil {
			PutEncodedFrame(old.Opus)
		}
		s.head = (s.head + 1) % maxAudioSourceCap
		s.n--
		evicted = true
	}
	s.buf[(s.head+s.n)%maxAudioSourceCap] = f
	s.n++
	s.adaptCapLocked(evicted)
	drop := s.drop
	s.mu.Unlock()
	if evicted && drop != nil {
		drop()
	}
	return ahead, evicted
}

func (s *SourceBuffer) adaptCapLocked(evicted bool) {
	if evicted {
		s.capacity = min(s.capacity+1, maxAudioSourceCap)
		s.peak, s.fed = 0, 0
		return
	}
	s.peak = max(s.peak, s.n)
	s.fed++
	if s.fed < sourceShrinkWindow {
		return
	}
	if s.peak < s.capacity {
		s.capacity = max(s.capacity-1, audioSourceCap)
	}
	s.peak, s.fed = 0, 0
}

func (s *SourceBuffer) bind(m *Mixer) {
	s.mu.Lock()
	s.mixer = m
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
	s.head = (s.head + 1) % maxAudioSourceCap
	s.n--
	s.mu.Unlock()
	return f, true
}

// Drain discards all buffered frames and returns their pool buffers.
func (s *SourceBuffer) Drain() {
	s.mu.Lock()
	for s.n > 0 {
		f := s.buf[s.head]
		s.head = (s.head + 1) % maxAudioSourceCap
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
