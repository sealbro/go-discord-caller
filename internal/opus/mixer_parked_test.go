package opus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// countingSource is an AudioSource of a kind the mixer cannot gate at Feed,
// so every look the mixer takes at its inputs shows up as a call.
type countingSource struct {
	pulls  atomic.Int64
	drains atomic.Int64
}

func (s *countingSource) Pull() (Frame, bool) { s.pulls.Add(1); return Frame{}, false }
func (s *countingSource) Drain()              { s.drains.Add(1) }
func (s *countingSource) Len() int            { return 0 }
func (s *countingSource) Close()              {}

func runMixer(t *testing.T, m *Mixer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newBareMixer(t *testing.T) *Mixer {
	t.Helper()
	m, err := NewMixer(telemetry.OpusRecorder{})
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	return m
}

// Most mixers of a raid are paused most of the time: every one in copy mode,
// and every destination nobody is listening in. Waking each of them 50 times
// a second to find nothing was a measurable share of the process's CPU.
func TestPausedMixerTicksSlowly(t *testing.T) {
	m := newBareMixer(t)
	src := &countingSource{}
	if err := m.AddInput(1, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	m.SetPaused(true)
	runMixer(t, m)

	const window = time.Second
	time.Sleep(window)

	atFrameRate := int64(window / mixerFrameDur)
	if got := src.drains.Load(); got > atFrameRate/4 {
		t.Errorf("paused mixer looked at its input %d times in %v; the frame rate is %d, a parked mixer should need about %d",
			got, window, atFrameRate, int64(window/parkedTickDur))
	}
}

func TestUnpauseWakesAParkedMixer(t *testing.T) {
	m := newBareMixer(t)
	src := &countingSource{}
	if err := m.AddInput(1, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	m.SetPaused(true)
	runMixer(t, m)
	// Past the first tick, so the mixer is waiting out a parked interval.
	time.Sleep(3 * mixerFrameDur)

	m.SetPaused(false)
	unpaused := time.Now()
	for src.pulls.Load() == 0 {
		if time.Since(unpaused) > parkedTickDur/2 {
			t.Fatalf("no tick within %v of unpausing: the mixer sat out its parked interval and the first words of the mix are lost", parkedTickDur/2)
		}
		time.Sleep(time.Millisecond)
	}
}

// A parked mixer no longer drains its inputs every 20 ms, so the frames fed to
// it meanwhile must not pile up in the ring: evicting them there counts each as
// a fanout drop, and a destination nobody listens in would read as frame loss.
func TestFeedIntoPausedMixerIsNotAnOverflow(t *testing.T) {
	m := newBareMixer(t)
	var overflows atomic.Int64
	src := NewSourceBuffer(func() { overflows.Add(1) })
	if err := m.AddInput(1, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	m.SetPaused(true)
	m.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())

	const fed = 4 * audioSourceCap
	for range fed {
		src.Feed(Frame{PCM: GetPCM(), Opus: CopyOpusFrame([]byte{1, 2, 3})})
	}

	if got := overflows.Load(); got != 0 {
		t.Errorf("%d frames fed to a paused mixer were counted as overflow drops, want 0", got)
	}
	if got := m.PausedDrops(); got != fed {
		t.Errorf("PausedDrops = %d, want %d", got, fed)
	}
	if got := src.Len(); got != 0 {
		t.Errorf("%d frames left buffered for a paused mixer, want 0", got)
	}
	// DrainWatcher lifts its own pause only when it sees audio arriving.
	if idle := m.IdleFor(); idle > time.Minute {
		t.Errorf("IdleFor = %v after frames arrived: audio reaching a paused mixer must count as activity", idle)
	}
}

// Every frame reaches the mixer with a pooled copy of its Opus packet, which
// only the single-source passthrough forwards. A mix has to hand them back.
func TestMixedFramesReturnTheirOpusBuffers(t *testing.T) {
	if raceDetector {
		t.Skip("sync.Pool drops buffers at random under the race detector")
	}
	m := newBareMixer(t)
	m.SetSink(PutEncodedFrame)
	a, b := NewSourceBuffer(nil), NewSourceBuffer(nil)
	if err := m.AddInput(1, a); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := m.AddInput(2, b); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	pcmFrame := sinePCM(440, 0, testAmplitude)
	// Mixing reads only the PCM; the packet just has to be pool-sized.
	packet := make([]byte, recvFrameCap/2)

	mixOnce := func() {
		for _, src := range []*SourceBuffer{a, b} {
			pcm := GetPCM()
			copy(pcm, pcmFrame)
			src.Feed(Frame{PCM: pcm, Opus: CopyOpusFrame(packet)})
		}
		if err := m.tick(); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	mixOnce()
	if allocs := testing.AllocsPerRun(200, mixOnce); allocs != 0 {
		t.Errorf("mixing two sources allocates %.0f times per tick, want 0: a pooled buffer is not being returned", allocs)
	}
}
