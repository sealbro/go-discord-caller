package pool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdentifyBackoffDoublesSpacingAndCaps(t *testing.T) {
	b := newIdentifyBackoff(0)
	now := time.Unix(0, 0)

	if got := b.reserve(now); !got.Equal(now) {
		t.Fatalf("first IDENTIFY must be immediate, got a wait of %v", got.Sub(now))
	}

	want := identifyBackoffBase
	for i := 0; i < 20; i++ {
		prev := b.last
		got := b.reserve(prev)
		if spacing := got.Sub(prev); spacing != min(want, identifyBackoffMax) {
			t.Fatalf("IDENTIFY %d: spacing %v, want %v", i+2, spacing, min(want, identifyBackoffMax))
		}
		want *= 2
	}
}

func TestIdentifyBackoffForgetsAfterStableStretch(t *testing.T) {
	b := newIdentifyBackoff(0)
	now := time.Unix(0, 0)
	for i := 0; i < 8; i++ {
		now = b.reserve(now)
	}

	later := now.Add(identifyStableAfter)
	if got := b.reserve(later); !got.Equal(later) {
		t.Fatalf("IDENTIFY after a stable stretch must be immediate, got a wait of %v", got.Sub(later))
	}
}

func TestIdentifyBackoffWait(t *testing.T) {
	t.Run("first call returns immediately", func(t *testing.T) {
		b := newIdentifyBackoff(0)
		if err := b.Wait(context.Background(), 0); err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
	})

	t.Run("second call waits out the spacing", func(t *testing.T) {
		b := newIdentifyBackoff(0)
		_ = b.Wait(context.Background(), 0)
		start := time.Now()
		if err := b.Wait(context.Background(), 0); err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
		if waited := time.Since(start); waited < identifyBackoffBase/2 {
			t.Errorf("second IDENTIFY waited only %v", waited)
		}
	})

	t.Run("cancelled context aborts the wait", func(t *testing.T) {
		b := newIdentifyBackoff(0)
		_ = b.Wait(context.Background(), 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := b.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want context.Canceled", err)
		}
	})
}
