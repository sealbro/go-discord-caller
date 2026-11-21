package opus

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/disgoorg/snowflake/v2"
)

// requireFrames fails unless exactly n frames were collected from index i on.
func requireFrames(t *testing.T, col *outputCollector, i, n int, phase string) [][]byte {
	t.Helper()
	got := col.from(i)
	if len(got) != n {
		t.Fatalf("%s: got %d output frames over %d ticks, want one per tick", phase, len(got), n)
	}
	return got
}

func TestMixerInputIDsFollowAddAndRemove(t *testing.T) {
	m := newBareMixer(t)
	m.AddInput(1, newSource())
	m.AddInput(2, newSource())
	m.AddInput(3, newSource())
	m.RemoveInput(2)

	got := m.InputIDs()
	slices.Sort(got)
	if want := []snowflake.ID{1, 3}; !slices.Equal(got, want) {
		t.Errorf("InputIDs = %v, want %v", got, want)
	}
}

// TestMixerSoloPassthrough verifies the single-source fast path:
// the mixer must forward the raw Opus packet byte-for-byte without re-encoding.
func TestMixerSoloPassthrough(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		frames := generateTestFrames(t, 440, 8, testAmplitude)
		ch := newSource()
		m.AddInput(newID(), ch)
		t.Cleanup(startPump(t.Context(), ch, frames))

		runTicks(8)
		got := requireFrames(t, col, 0, 8, "solo")

		for i, pkt := range got {
			if !matchesAnyFrame(pkt, frames) {
				t.Errorf("frame %d: not byte-equal to any input Opus packet — passthrough optimization not active", i)
			}
		}

		pcm := decodeOpusFrames(t, got)
		if rms := rmsInt16(pcm); rms < 500 {
			t.Errorf("solo passthrough: decoded RMS %.1f below threshold 500 (silence or corrupt output)", rms)
		}
	})
}

// TestMixerMultipleSpeakers verifies the mix path: output must be re-encoded
// (not byte-equal to any source), audible, and free of hard clipping.
func TestMixerMultipleSpeakers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		freqs     []int
		amplitude int
		minRMS    float64
	}{
		{name: "two", freqs: []int{440, 880}, amplitude: testAmplitude, minRMS: 500},
		{name: "three", freqs: []int{440, 880, 1320}, amplitude: testAmplitude3, minRMS: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m, col := newTestMixer(t)

				var sources [][]testFrame
				for _, freq := range tc.freqs {
					frames := generateTestFrames(t, freq, 8, tc.amplitude)
					sources = append(sources, frames)
					ch := newSource()
					m.AddInput(newID(), ch)
					t.Cleanup(startPump(t.Context(), ch, frames))
				}

				runTicks(10)
				got := requireFrames(t, col, 0, 10, tc.name+" speakers")

				for i, pkt := range got {
					for s, frames := range sources {
						if matchesAnyFrame(pkt, frames) {
							t.Errorf("frame %d: matches source %d verbatim — mixer skipped re-encode", i, s)
						}
					}
				}

				pcm := decodeOpusFrames(t, got)
				if rms := rmsInt16(pcm); rms < tc.minRMS {
					t.Errorf("decoded RMS %.1f below threshold %.0f (silence or corrupt output)", rms, tc.minRMS)
				}
				if peak := maxAbsInt16(pcm); peak == 32767 {
					t.Errorf("hard clipping detected — check PCM accumulator clamping (amplitude=%d, %d sources)", tc.amplitude, len(tc.freqs))
				}
			})
		})
	}
}

// TestMixerJoinMidStream verifies the passthrough→mix transition:
// solo output must be byte-identical to the source; after a second source joins,
// output must switch to re-encoded frames from the very next tick.
func TestMixerJoinMidStream(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		framesA := generateTestFrames(t, 440, 8, testAmplitude)
		framesB := generateTestFrames(t, 880, 8, testAmplitude)

		chA := newSource()
		m.AddInput(newID(), chA)
		t.Cleanup(startPump(t.Context(), chA, framesA))

		runTicks(5)
		for i, pkt := range requireFrames(t, col, 0, 5, "phase 1") {
			if !matchesAnyFrame(pkt, framesA) {
				t.Errorf("phase 1 frame %d: not a passthrough of source A (solo fast-path broken)", i)
			}
		}

		chB := newSource()
		m.AddInput(newID(), chB)
		t.Cleanup(startPump(t.Context(), chB, framesB))

		runTicks(5)
		phase2 := requireFrames(t, col, 5, 5, "phase 2")
		for i, pkt := range phase2 {
			if matchesAnyFrame(pkt, framesA) {
				t.Errorf("phase 2 frame %d: still passing source A through after B joined — mix not active", i)
			}
			if matchesAnyFrame(pkt, framesB) {
				t.Errorf("phase 2 frame %d: still passing source B through after joining — mix not active", i)
			}
		}
		pcm := decodeOpusFrames(t, phase2)
		if rms := rmsInt16(pcm); rms < 500 {
			t.Errorf("phase 2: decoded RMS %.1f too low after second speaker joined", rms)
		}
	})
}

// TestMixerLeaveMidStream verifies the mix→passthrough transition:
// while two sources are active the output is re-encoded; once one leaves,
// the mixer must revert to byte-identical passthrough for the remaining source.
func TestMixerLeaveMidStream(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		framesA := generateTestFrames(t, 440, 8, testAmplitude)
		framesB := generateTestFrames(t, 880, 8, testAmplitude)

		chA, chB := newSource(), newSource()
		idB := newID()
		m.AddInput(newID(), chA)
		m.AddInput(idB, chB)
		t.Cleanup(startPump(t.Context(), chA, framesA))
		stopB := startPump(t.Context(), chB, framesB)
		t.Cleanup(stopB)

		runTicks(5)
		requireFrames(t, col, 0, 5, "warmup")

		m.RemoveInput(idB)
		stopB()

		runTicks(5)
		for i, pkt := range requireFrames(t, col, 5, 5, "leave") {
			if !matchesAnyFrame(pkt, framesA) {
				t.Errorf("post-leave frame %d: not a passthrough of source A (passthrough not resumed after B left)", i)
			}
		}
	})
}

// TestMixerRapidJoinLeave verifies that the mixer handles multiple mode transitions
// (passthrough→mix→passthrough) without producing silence or corrupted frames.
func TestMixerRapidJoinLeave(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, col := newTestMixer(t)

		framesA := generateTestFrames(t, 440, 8, testAmplitude)
		framesB := generateTestFrames(t, 660, 8, testAmplitude)
		framesC := generateTestFrames(t, 880, 8, testAmplitude)

		chA := newSource()
		m.AddInput(newID(), chA)
		t.Cleanup(startPump(t.Context(), chA, framesA))
		runTicks(3)
		requireFrames(t, col, 0, 3, "phase 1")

		chB, chC := newSource(), newSource()
		idB, idC := newID(), newID()
		m.AddInput(idB, chB)
		m.AddInput(idC, chC)
		stopB := startPump(t.Context(), chB, framesB)
		stopC := startPump(t.Context(), chC, framesC)
		t.Cleanup(stopB)
		t.Cleanup(stopC)
		runTicks(3)
		for i, pkt := range requireFrames(t, col, 3, 3, "phase 2") {
			if matchesAnyFrame(pkt, framesA) {
				t.Errorf("phase 2 frame %d: passthrough of source A while three speak", i)
			}
		}

		m.RemoveInput(idB)
		m.RemoveInput(idC)
		stopB()
		stopC()
		runTicks(5)
		for i, pkt := range requireFrames(t, col, 6, 5, "phase 3") {
			if !matchesAnyFrame(pkt, framesA) {
				t.Errorf("phase 3 frame %d: not passthrough of source A after B+C left", i)
			}
		}

		pcm := decodeOpusFrames(t, col.from(0))
		if rms := rmsInt16(pcm); rms < 100 {
			t.Errorf("overall RMS %.1f too low — possible silence or dropout during mode transitions", rms)
		}
	})
}
