package opus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSourceBufferFeedPullOrder verifies FIFO ordering of Feed → Pull.
func TestSourceBufferFeedPullOrder(t *testing.T) {
	t.Parallel()
	s := NewSourceBuffer(nil)
	for i := range audioSourceCap {
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Unix(0, int64(i+1))})
	}
	for i := range audioSourceCap {
		f, ok := s.Pull()
		if !ok {
			t.Fatalf("Pull %d: underrun before drain", i)
		}
		if got := f.CreatedAt.UnixNano(); got != int64(i+1) {
			t.Fatalf("Pull %d: out-of-order frame; want %d got %d", i, i+1, got)
		}
		PutPCM(f.PCM)
		PutEncodedFrame(f.Opus)
	}
	if _, ok := s.Pull(); ok {
		t.Fatal("Pull after drain returned a frame")
	}
}

// TestSourceBufferOverflowEvictsOldest checks that Feed silently drops the
// oldest entry on overflow and calls the drop callback.
func TestSourceBufferOverflowEvictsOldest(t *testing.T) {
	t.Parallel()
	var drops atomic.Int32
	s := NewSourceBuffer(func() { drops.Add(1) })

	// Fill + overflow by one frame.
	for i := range audioSourceCap + 1 {
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Unix(0, int64(i+1))})
	}
	if got := drops.Load(); got != 1 {
		t.Fatalf("expected 1 drop after one overflow; got %d", got)
	}

	// Oldest (i=1) was evicted; surviving frames start at i=2.
	wantStart := int64(2)
	for i := range audioSourceCap {
		f, ok := s.Pull()
		if !ok {
			t.Fatalf("Pull %d: underrun before drain", i)
		}
		if got := f.CreatedAt.UnixNano(); got != wantStart+int64(i) {
			t.Fatalf("Pull %d: want %d got %d", i, wantStart+int64(i), got)
		}
		PutPCM(f.PCM)
		PutEncodedFrame(f.Opus)
	}
}

// TestSourceBufferDrain verifies Drain empties the ring and returns pool buffers.
func TestSourceBufferDrain(t *testing.T) {
	t.Parallel()
	s := NewSourceBuffer(nil)
	for i := range audioSourceCap {
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Unix(0, int64(i))})
	}
	s.Drain()
	if _, ok := s.Pull(); ok {
		t.Fatal("Pull after Drain returned a frame")
	}
}

// TestSourceBufferNilDropCallback ensures a nil drop func does not panic on overflow.
func TestSourceBufferNilDropCallback(t *testing.T) {
	t.Parallel()
	s := NewSourceBuffer(nil)
	for i := range audioSourceCap + 2 {
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Unix(0, int64(i))})
	}
	// Drain to return pool buffers; assertion is "did not panic".
	s.Drain()
}

// BenchmarkSourceBufferFeedPullContended runs one producer at 50 Hz feeding
// frames and one consumer at 50 Hz pulling them, mirroring the runtime hot
// path (VoiceReceiver fanout → mixer tick). Regression guard for lock
// contention in SourceBuffer.
//
// b.N controls the number of frames the producer pushes. The consumer keeps
// up by pulling at the same rate; on overflow Feed silently evicts the oldest.
func BenchmarkSourceBufferFeedPullContended(b *testing.B) {
	s := NewSourceBuffer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if f, ok := s.Pull(); ok {
					PutPCM(f.PCM)
					PutEncodedFrame(f.Opus)
				}
			}
		}
	})

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	b.ResetTimer()
	for b.Loop() {
		<-ticker.C
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Now()})
	}
	b.StopTimer()

	cancel()
	wg.Wait()
	s.Drain()
}

// BenchmarkSourceBufferFeed measures Feed-only throughput with no consumer,
// exercising the overflow eviction path under maximum producer pressure.
// Captures the worst case for the per-Feed mutex cost when the ring is
// constantly full.
func BenchmarkSourceBufferFeed(b *testing.B) {
	s := NewSourceBuffer(nil)
	b.ResetTimer()
	for b.Loop() {
		s.Feed(Frame{PCM: GetPCM(), Opus: getEncodedFrame(8), CreatedAt: time.Now()})
	}
	b.StopTimer()
	s.Drain()
}
