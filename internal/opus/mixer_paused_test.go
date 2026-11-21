package opus

import (
	"testing"
	"testing/synctest"
	"time"
)

// TestMixerPausedDropsAccumulate verifies that frames fed into a paused
// mixer are counted via PausedDrops and produce no output. This is the
// signal used to diagnose the "upstream is feeding, downstream is silently
// dropping" case (e.g. a host channel mixer paused while the relay bridge
// continues to deliver guest packets).
func TestMixerPausedDropsAccumulate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)
		frames := generateTestFrames(t, 440, audioSourceCap, testAmplitude)

		src := newSource()
		m.AddInput(newID(), src)

		m.SetPaused(true)
		if !m.Paused() {
			t.Fatal("Paused() must report true after SetPaused(true)")
		}

		for _, f := range frames {
			pcm := GetPCM()
			copy(pcm, f.pcm)
			opusCopy := make([]byte, len(f.opus))
			copy(opusCopy, f.opus)
			src.Feed(Frame{PCM: pcm, Opus: opusCopy, CreatedAt: time.Now()})
		}

		runTicks(int(parkedTickDur/mixerFrameDur) + 1)
		if got, want := m.PausedDrops(), uint64(len(frames)); got != want {
			t.Errorf("PausedDrops = %d, want %d", got, want)
		}
		if col.count() != 0 {
			t.Errorf("paused mixer produced %d output frames; want 0", col.count())
		}
	})
}

// The router calls SetPaused(false) on every running destination at every
// recompute. Only a real paused → running transition may reset the activity
// clock, or a recompute would hide an idle mixer from DrainWatcher.
func TestMixerSetPausedFalseOnRunningMixerKeepsIdleTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newBareMixer(t)
		m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		m.SetPaused(false)

		if got := m.IdleFor(); got != time.Hour {
			t.Errorf("IdleFor = %v after SetPaused(false) on a running mixer, want 1h", got)
		}
	})
}

// TestMixerUnpauseResetsActivityTimestamp guards the §3.5 fix: when a mixer
// has been paused long enough that IdleFor would exceed DrainIdleTimeout,
// SetPaused(false) must reset lastActivityAt so DrainWatcher doesn't insta-
// pause it before it has had a chance to consume a single frame.
func TestMixerUnpauseResetsActivityTimestamp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newBareMixer(t)

		m.SetPaused(true)
		m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		m.SetPaused(false)

		if got := m.IdleFor(); got != 0 {
			t.Errorf("SetPaused(false) should reset lastActivityAt; IdleFor=%v, want 0", got)
		}
	})
}
