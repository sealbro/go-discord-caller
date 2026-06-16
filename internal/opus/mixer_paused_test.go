package opus

import (
	"testing"
	"time"
)

// TestMixerPausedDropsAccumulate verifies that frames fed into a paused
// mixer are counted via PausedDrops and produce no output. This is the
// signal used to diagnose the "upstream is feeding, downstream is silently
// dropping" case (e.g. a host channel mixer paused while the relay bridge
// continues to deliver guest packets).
func TestMixerPausedDropsAccumulate(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	// audioSourceCap caps the ring; feeding more would silently evict at the
	// SourceBuffer (not the mixer) and not register as a paused-drop.
	frames := generateTestFrames(t, 440, audioSourceCap, testAmplitude)

	src := newSource()
	if err := m.AddInput(newID(), src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	m.SetPaused(true)
	if !m.Paused() {
		t.Fatal("Paused() must report true after SetPaused(true)")
	}

	// Feed a few frames; the next tick must discard them.
	for _, f := range frames {
		pcm := GetPCM()
		copy(pcm, f.pcm)
		opusCopy := make([]byte, len(f.opus))
		copy(opusCopy, f.opus)
		src.Feed(Frame{PCM: pcm, Opus: opusCopy, CreatedAt: time.Now()})
	}

	// Give the tick a few cycles to observe and drain the buffer.
	deadline := time.Now().Add(collectTimeout)
	for time.Now().Before(deadline) {
		if m.PausedDrops() >= uint64(len(frames)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := m.PausedDrops(), uint64(len(frames)); got < want {
		t.Errorf("PausedDrops: want >= %d got %d", want, got)
	}
	if col.count() != 0 {
		t.Errorf("paused mixer produced %d output frames; want 0", col.count())
	}
}

// TestMixerPausedTransitionLog is a behavioural smoke test for SetPaused:
// repeated SetPaused(p) with the same p must not double-increment the
// transition log (no observable assertion; we only check the no-op shape).
func TestMixerPausedTransitionIdempotent(t *testing.T) {
	t.Parallel()
	m, _ := newTestMixer(t)
	if m.Paused() {
		t.Fatal("new mixer should not be paused")
	}
	m.SetPaused(false) // already false — should be a no-op
	m.SetPaused(true)
	m.SetPaused(true) // repeated — should be a no-op
	if !m.Paused() {
		t.Fatal("Paused must report true after SetPaused(true)")
	}
	m.SetPaused(false)
	if m.Paused() {
		t.Fatal("Paused must report false after SetPaused(false)")
	}
}

// TestMixerUnpauseResetsActivityTimestamp guards the §3.5 fix: when a mixer
// has been paused long enough that IdleFor would exceed DrainIdleTimeout,
// SetPaused(false) must reset lastActivityAt so DrainWatcher doesn't insta-
// pause it before it has had a chance to consume a single frame.
func TestMixerUnpauseResetsActivityTimestamp(t *testing.T) {
	t.Parallel()
	m, _ := newTestMixer(t)

	m.SetPaused(true)
	// Backdate lastActivityAt so IdleFor looks like an eternity has passed.
	m.lastActivityAt.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	if got := m.IdleFor(); got < 30*time.Minute {
		t.Fatalf("backdated IdleFor should be ~1h, got %v", got)
	}

	m.SetPaused(false)

	if got := m.IdleFor(); got > 100*time.Millisecond {
		t.Errorf("SetPaused(false) should reset lastActivityAt; IdleFor=%v, want < 100ms", got)
	}
}
