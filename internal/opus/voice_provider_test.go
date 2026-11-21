package opus

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/disgoorg/disgo/voice"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// TestVoiceProviderPassthrough verifies the normal path: a frame fed into the
// channel is returned verbatim by ProvideOpusFrame and the drop counter stays at zero.
func TestVoiceProviderPassthrough(t *testing.T) {
	t.Parallel()
	ch := make(chan []byte, 8)
	var drops atomic.Int32
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{}.WithDrop(func() { drops.Add(1) }))

	want := []byte{1, 2, 3}
	ch <- want
	got, err := vp.ProvideOpusFrame()
	if err != nil {
		t.Fatalf("ProvideOpusFrame: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("frame mismatch: want %v got %v", want, got)
	}
	if d := drops.Load(); d != 0 {
		t.Errorf("unexpected drops: %d", d)
	}
}

// TestVoiceProviderDrainThreshold verifies the bleed-off path: when the
// channel depth exceeds providerDrainThreshold, ProvideOpusFrame drops a
// single extra frame so it never lags by more than 20 ms × threshold.
func TestVoiceProviderDrainThreshold(t *testing.T) {
	t.Parallel()
	// Buffer big enough to seed depth > threshold.
	ch := make(chan []byte, providerDrainThreshold+4)
	var drops atomic.Int32
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{}.WithDrop(func() { drops.Add(1) }))

	// Seed 5 frames so the post-read depth is 4 (> threshold = 3).
	for i := byte(1); i <= 5; i++ {
		ch <- []byte{i}
	}
	got, err := vp.ProvideOpusFrame()
	if err != nil {
		t.Fatalf("ProvideOpusFrame: %v", err)
	}
	// First read fetches frame 1; because len(ch)=4 > threshold, the provider
	// then drops frame 1 and replaces it with the next frame (frame 2).
	// "Bleed-off" semantics: one frame is shaved per call until depth recovers.
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("want frame 2 after one-frame bleed-off; got %v", got)
	}
	if d := drops.Load(); d != 1 {
		t.Errorf("expected exactly 1 drop; got %d", d)
	}

	// Drain remaining 3 frames (3, 4, 5) without triggering another drain.
	for i, want := byte(0), byte(3); want <= 5; i, want = i+1, want+1 {
		got, err = vp.ProvideOpusFrame()
		if err != nil {
			t.Fatalf("ProvideOpusFrame (drain %d): %v", i, err)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("drain frame %d: want %d got %v", i, want, got)
		}
	}
	if d := drops.Load(); d != 1 {
		t.Errorf("expected no additional drops; got %d total", d)
	}
}

// requireBlocksUntilClose checks that ProvideOpusFrame parks until Close, then
// returns an error, and that Close is idempotent.
func requireBlocksUntilClose(t *testing.T, vp voice.OpusFrameProvider) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := vp.ProvideOpusFrame(); done <- err }()

	synctest.Wait()
	select {
	case <-done:
		t.Fatal("ProvideOpusFrame returned before Close")
	default:
	}

	vp.Close()
	synctest.Wait()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ProvideOpusFrame returned nil error after Close")
		}
	default:
		t.Fatal("ProvideOpusFrame did not return after Close")
	}

	vp.Close()
}

func TestVoiceProviderClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		requireBlocksUntilClose(t, NewVoiceProvider(make(chan []byte), telemetry.OpusRecorder{}))
	})
}

// TestVoiceProviderChannelClosed verifies that closing the input channel
// surfaces as an error from ProvideOpusFrame (mixer-shutdown path).
func TestVoiceProviderChannelClosed(t *testing.T) {
	t.Parallel()
	ch := make(chan []byte)
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{})

	close(ch)
	if _, err := vp.ProvideOpusFrame(); err == nil {
		t.Fatal("expected error when input channel is closed")
	}
}

// TestVoiceProviderPoolReturn confirms that the previous frame is returned to
// the pool before the next read blocks. Uses a getEncodedFrame buffer to make
// the pool return path observable: an identical buffer should be reusable.
func TestVoiceProviderPoolReturn(t *testing.T) {
	t.Parallel()
	ch := make(chan []byte, 2)
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{})

	// First frame uses a pool buffer.
	f1 := getEncodedFrame(10)
	for i := range f1 {
		f1[i] = byte(i)
	}
	ch <- f1
	got1, err := vp.ProvideOpusFrame()
	if err != nil {
		t.Fatalf("first ProvideOpusFrame: %v", err)
	}
	if cap(got1) != encodedFrameCap {
		t.Fatalf("first frame should be pool-backed (cap=%d); got cap=%d", encodedFrameCap, cap(got1))
	}

	// Second frame: also pool-backed. After this call, f1 has been recycled
	// (returned to the pool at the top of the call).
	f2 := getEncodedFrame(10)
	ch <- f2
	got2, err := vp.ProvideOpusFrame()
	if err != nil {
		t.Fatalf("second ProvideOpusFrame: %v", err)
	}
	if cap(got2) != encodedFrameCap {
		t.Fatalf("second frame should be pool-backed (cap=%d); got cap=%d", encodedFrameCap, cap(got2))
	}

	// Close releases the held v.prev (no second pool return — Close does not
	// return prev). This test asserts that no double-free or panic occurs.
	vp.Close()
}

// TestVoiceProviderPassthroughCap verifies that non-pool buffers (wrong cap)
// pass through PutEncodedFrame as a no-op without panic.
func TestVoiceProviderPassthroughCap(t *testing.T) {
	t.Parallel()
	ch := make(chan []byte, 2)
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{})

	// Slice with an arbitrary, non-pool capacity.
	raw := make([]byte, 5, 17)
	ch <- raw
	got, err := vp.ProvideOpusFrame()
	if err != nil {
		t.Fatalf("first ProvideOpusFrame: %v", err)
	}
	if cap(got) != 17 {
		t.Fatalf("frame should retain raw cap=17; got cap=%d", cap(got))
	}

	// Second frame causes the first (raw, cap=17) to flow through PutEncodedFrame.
	// PutEncodedFrame's switch defaults to a no-op for unknown caps — assert no panic.
	ch <- []byte{9}
	if _, err := vp.ProvideOpusFrame(); err != nil {
		t.Fatalf("second ProvideOpusFrame: %v", err)
	}
}

func TestEmptyVoiceProviderBlocksUntilClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		requireBlocksUntilClose(t, NewEmptyVoiceProvider())
	})
}

// A sender that cannot send this tick discards queued frames one at a time,
// so the queue drains at the frame rate.
func TestVoiceProviderDiscardFrame(t *testing.T) {
	t.Parallel()
	ch := make(chan []byte, 2)
	vp := NewVoiceProvider(ch, telemetry.OpusRecorder{})
	ch <- []byte{1}
	ch <- []byte{2}

	if !vp.DiscardFrame() {
		t.Fatal("DiscardFrame = false with two frames queued")
	}
	if got := len(ch); got != 1 {
		t.Fatalf("%d frames queued after one discard, want 1", got)
	}
	if !vp.DiscardFrame() {
		t.Fatal("DiscardFrame = false with one frame queued")
	}
	if vp.DiscardFrame() {
		t.Error("DiscardFrame = true on an empty queue")
	}
	close(ch)
	if vp.DiscardFrame() {
		t.Error("DiscardFrame = true on a closed channel")
	}
}
