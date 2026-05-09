package opus

import (
	"context"
	"testing"
	"time"
)

const collectTimeout = 3 * time.Second

// TestMixerSoloPassthrough verifies the single-source fast path:
// the mixer must forward the raw Opus packet byte-for-byte without re-encoding.
func TestMixerSoloPassthrough(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	frames := generateTestFrames(t, 440, 8, testAmplitude)
	ch := newSourceChan()
	m.AddInput(newID(), ch)
	stop := startPump(ctx, ch, frames)
	t.Cleanup(stop)

	got := col.waitForN(8, collectTimeout)
	if got == nil {
		t.Fatal("timed out waiting for 8 output frames")
	}

	for i, pkt := range got {
		if !matchesAnyFrame(pkt, frames) {
			t.Errorf("frame %d: not byte-equal to any input Opus packet — passthrough optimization not active", i)
		}
	}

	pcm := decodeOpusFrames(t, got)
	if rms := rmsInt16(pcm); rms < 500 {
		t.Errorf("solo passthrough: decoded RMS %.1f below threshold 500 (silence or corrupt output)", rms)
	}
}

// TestMixerTwoSpeakers verifies the two-source mix path:
// output must be re-encoded (not byte-equal to either source), audible, and free of hard clipping.
func TestMixerTwoSpeakers(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	framesA := generateTestFrames(t, 440, 8, testAmplitude)
	framesB := generateTestFrames(t, 880, 8, testAmplitude)

	chA, chB := newSourceChan(), newSourceChan()
	m.AddInput(newID(), chA)
	m.AddInput(newID(), chB)
	stopA := startPump(ctx, chA, framesA)
	stopB := startPump(ctx, chB, framesB)
	t.Cleanup(stopA)
	t.Cleanup(stopB)

	got := col.waitForN(10, collectTimeout)
	if got == nil {
		t.Fatal("timed out waiting for 10 output frames")
	}

	for i, pkt := range got {
		if matchesAnyFrame(pkt, framesA) {
			t.Errorf("frame %d: matches source A verbatim — mixer skipped re-encode for two-source mix", i)
		}
		if matchesAnyFrame(pkt, framesB) {
			t.Errorf("frame %d: matches source B verbatim — mixer skipped re-encode for two-source mix", i)
		}
	}

	pcm := decodeOpusFrames(t, got)
	if rms := rmsInt16(pcm); rms < 500 {
		t.Errorf("two speakers: decoded RMS %.1f below threshold 500 (silence or corrupt output)", rms)
	}
	if peak := maxAbsInt16(pcm); peak == 32767 {
		t.Errorf("two speakers: hard clipping detected — check PCM accumulator clamping (testAmplitude=%d, 2 sources)", testAmplitude)
	}
}

// TestMixerThreeSpeakers verifies the three-source mix path: audible output with no hard clipping.
// Uses testAmplitude3 (10000) so three sources sum to at most 30000, safely below the int16 ceiling.
func TestMixerThreeSpeakers(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	for _, freq := range []int{440, 880, 1320} {
		frames := generateTestFrames(t, freq, 8, testAmplitude3)
		ch := newSourceChan()
		m.AddInput(newID(), ch)
		stop := startPump(ctx, ch, frames)
		t.Cleanup(stop)
	}

	got := col.waitForN(10, collectTimeout)
	if got == nil {
		t.Fatal("timed out waiting for 10 output frames")
	}

	pcm := decodeOpusFrames(t, got)
	if rms := rmsInt16(pcm); rms < 300 {
		t.Errorf("three speakers: decoded RMS %.1f below threshold 300", rms)
	}
	if peak := maxAbsInt16(pcm); peak == 32767 {
		t.Errorf("three speakers: hard clipping detected — PCM sum exceeded int16 range (testAmplitude3=%d, 3 sources)", testAmplitude3)
	}
}

// TestMixerJoinMidStream verifies the passthrough→mix transition:
// solo output must be byte-identical to the source; after a second source joins,
// output must switch to re-encoded frames.
func TestMixerJoinMidStream(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	framesA := generateTestFrames(t, 440, 8, testAmplitude)
	framesB := generateTestFrames(t, 880, 8, testAmplitude)

	// Phase 1: solo source A — passthrough must be active.
	chA := newSourceChan()
	m.AddInput(newID(), chA)
	stopA := startPump(ctx, chA, framesA)
	t.Cleanup(stopA)

	phase1 := col.waitForN(5, collectTimeout)
	if phase1 == nil {
		t.Fatal("phase 1: timed out waiting for 5 solo frames")
	}
	for i, pkt := range phase1 {
		if !matchesAnyFrame(pkt, framesA) {
			t.Errorf("phase 1 frame %d: not a passthrough of source A (solo fast-path broken)", i)
		}
	}

	// Phase 2: add source B and allow a few ticks for the mixer to see both inputs.
	chB := newSourceChan()
	m.AddInput(newID(), chB)
	stopB := startPump(ctx, chB, framesB)
	t.Cleanup(stopB)
	time.Sleep(60 * time.Millisecond)

	baseline := col.count()
	phase2 := col.collectFrom(baseline, 5, collectTimeout)
	if phase2 == nil {
		t.Fatal("phase 2: timed out waiting for 5 mixed frames after join")
	}
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
}

// TestMixerLeaveMidStream verifies the mix→passthrough transition:
// while two sources are active the output is re-encoded; once one leaves,
// the mixer must revert to byte-identical passthrough for the remaining source.
func TestMixerLeaveMidStream(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	framesA := generateTestFrames(t, 440, 8, testAmplitude)
	framesB := generateTestFrames(t, 880, 8, testAmplitude)

	chA, chB := newSourceChan(), newSourceChan()
	idB := newID()
	m.AddInput(newID(), chA)
	m.AddInput(idB, chB)
	stopA := startPump(ctx, chA, framesA)
	stopB := startPump(ctx, chB, framesB)
	t.Cleanup(stopA)
	t.Cleanup(stopB)

	// Warm up: let the mixer produce a few mixed frames.
	if col.waitForN(5, collectTimeout) == nil {
		t.Fatal("warmup: timed out waiting for 5 mixed frames")
	}

	// Remove source B; allow a couple of ticks before collecting.
	m.RemoveInput(idB)
	stopB()
	time.Sleep(60 * time.Millisecond)

	baseline := col.count()
	phase2 := col.collectFrom(baseline, 5, collectTimeout)
	if phase2 == nil {
		t.Fatal("leave: timed out waiting for 5 post-leave frames")
	}
	for i, pkt := range phase2 {
		if !matchesAnyFrame(pkt, framesA) {
			t.Errorf("post-leave frame %d: not a passthrough of source A (passthrough not resumed after B left)", i)
		}
	}
}

// TestMixerRapidJoinLeave verifies that the mixer handles multiple mode transitions
// (passthrough→mix→passthrough) without producing silence or corrupted frames.
func TestMixerRapidJoinLeave(t *testing.T) {
	t.Parallel()
	m, col := newTestMixer(t)
	ctx := context.Background()

	framesA := generateTestFrames(t, 440, 8, testAmplitude)
	framesB := generateTestFrames(t, 660, 8, testAmplitude)
	framesC := generateTestFrames(t, 880, 8, testAmplitude)

	// Phase 1: solo A.
	chA := newSourceChan()
	m.AddInput(newID(), chA)
	stopA := startPump(ctx, chA, framesA)
	t.Cleanup(stopA)
	if col.waitForN(3, collectTimeout) == nil {
		t.Fatal("phase 1: timed out")
	}

	// Phase 2: three simultaneous speakers.
	chB, chC := newSourceChan(), newSourceChan()
	idB, idC := newID(), newID()
	m.AddInput(idB, chB)
	m.AddInput(idC, chC)
	stopB := startPump(ctx, chB, framesB)
	stopC := startPump(ctx, chC, framesC)
	t.Cleanup(stopB)
	t.Cleanup(stopC)
	time.Sleep(60 * time.Millisecond)
	baseline2 := col.count()
	if col.collectFrom(baseline2, 3, collectTimeout) == nil {
		t.Fatal("phase 2: timed out waiting for mixed frames")
	}

	// Phase 3: remove B and C, back to solo A.
	m.RemoveInput(idB)
	m.RemoveInput(idC)
	stopB()
	stopC()
	time.Sleep(60 * time.Millisecond)

	baseline3 := col.count()
	phase3 := col.collectFrom(baseline3, 5, collectTimeout)
	if phase3 == nil {
		t.Fatal("phase 3: timed out waiting for passthrough frames after B+C left")
	}
	for i, pkt := range phase3 {
		if !matchesAnyFrame(pkt, framesA) {
			t.Errorf("phase 3 frame %d: not passthrough of source A after B+C left", i)
		}
	}

	// Overall: no silence throughout all phases.
	all := col.waitForN(col.count(), time.Millisecond)
	if all != nil {
		pcm := decodeOpusFrames(t, all)
		if rms := rmsInt16(pcm); rms < 100 {
			t.Errorf("overall RMS %.1f too low — possible silence or dropout during mode transitions", rms)
		}
	}
}
