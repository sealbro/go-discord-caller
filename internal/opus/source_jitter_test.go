package opus

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// TestSourceBufferAbsorbsSustainedJitter drives a mixer input with the arrival
// pattern of a caller on a distant voice server: steady 20 ms frames, then the
// network holds three back and delivers them together with the next. In
// production (a guild whose owner bot sat on a voice server ~140 ms from the
// host) a quarter of frames arrived over 30 ms late and 10% of the audio was
// evicted, because a fixed 3-frame input cannot hold a 4-frame burst. Once the
// jitter has been seen for a few seconds, the input should hold enough to play
// every frame rather than evicting one per burst.
func TestSourceBufferAbsorbsSustainedJitter(t *testing.T) {
	t.Parallel()

	const (
		cycle     = 25 // ticks per hold-and-burst cycle: 25 frames in, 25 ticks out
		held      = 3  // frames the network holds back each cycle (60 ms)
		ticks     = 1500
		warmupEnd = 250 // 5 s to learn the jitter before losses count
	)
	arrivals := func(tick int) int {
		switch pos := tick % cycle; {
		case pos >= cycle-held-1 && pos < cycle-1:
			return 0
		case pos == cycle-1:
			return held + 1
		default:
			return 1
		}
	}

	m, err := NewMixer(telemetry.OpusRecorder{})
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	var played []uint32
	m.SetSink(func(pkt []byte) { played = append(played, binary.BigEndian.Uint32(pkt)) })
	src := NewSourceBuffer(nil)
	m.AddInput(newID(), src)

	var seq uint32
	var firstCounted uint32
	for tick := range ticks {
		if tick == warmupEnd {
			firstCounted = seq
		}
		for range arrivals(tick) {
			pkt := getEncodedFrame(8)
			binary.BigEndian.PutUint32(pkt, seq)
			src.Feed(Frame{PCM: GetPCM(), Opus: pkt, CreatedAt: time.Now()})
			seq++
		}
		if err := m.tick(); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}

	want := firstCounted
	var missing []uint32
	for _, s := range played {
		if s < firstCounted {
			continue
		}
		for ; want < s; want++ {
			missing = append(missing, want)
		}
		want = s + 1
	}
	if len(missing) > 0 {
		counted := seq - firstCounted
		t.Fatalf("after %d s of the same jitter, %d of %d frames were never played (%.1f%%), e.g. %v: the input evicts one frame per burst instead of growing to hold it",
			warmupEnd/50, len(missing), counted, 100*float64(len(missing))/float64(counted), missing[:min(5, len(missing))])
	}
}

// talkSpurt models speech the way Discord delivers it: 1 s of 20 ms frames,
// then 0.5 s of silence in which nothing is sent.
func talkSpurt(tick int) bool { return tick%75 < 50 }

// driveSource feeds src from arrivals and pulls one frame per tick, as the
// mixer does, for the given number of ticks.
func driveSource(src *SourceBuffer, ticks int, arrivals func(tick int) int) {
	for tick := range ticks {
		for range arrivals(tick) {
			src.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Now()})
		}
		if f, ok := src.Pull(); ok {
			PutPCM(f.PCM)
			PutEncodedFrame(f.Opus)
		}
	}
}

// TestSourceBufferKeepsCapacityForOrdinaryJitter guards the other side of
// growing: a nearby caller whose frames now and then arrive one tick late
// must keep the starting capacity, or every input would gain delay.
func TestSourceBufferKeepsCapacityForOrdinaryJitter(t *testing.T) {
	t.Parallel()
	src := NewSourceBuffer(nil)
	driveSource(src, 3000, func(tick int) int {
		switch {
		case !talkSpurt(tick):
			return 0
		case tick%10 == 4:
			return 0
		case tick%10 == 5:
			return 2
		}
		return 1
	})
	if src.capacity != audioSourceCap {
		t.Fatalf("capacity = %d after a minute of 1-frame jitter, want %d", src.capacity, audioSourceCap)
	}
}

// TestSourceBufferShrinksWhenJitterStops checks that capacity grown for a
// jittery stretch is given back once the bursts stop, so the extra delay does
// not outlive the network trouble that needed it.
func TestSourceBufferShrinksWhenJitterStops(t *testing.T) {
	t.Parallel()
	src := NewSourceBuffer(nil)
	burst := func(tick int) int {
		switch pos := tick % 25; {
		case pos >= 21 && pos < 24:
			return 0
		case pos == 24:
			return 4
		}
		return 1
	}
	driveSource(src, 500, burst)
	if src.capacity <= audioSourceCap {
		t.Fatalf("capacity = %d after 10 s of 4-frame bursts, want it grown past %d", src.capacity, audioSourceCap)
	}
	driveSource(src, 3000, func(tick int) int {
		if talkSpurt(tick) {
			return 1
		}
		return 0
	})
	if src.capacity != audioSourceCap {
		t.Fatalf("capacity = %d after a minute without bursts, want it back to %d", src.capacity, audioSourceCap)
	}
}
