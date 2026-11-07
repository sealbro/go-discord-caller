package pool

import (
	"testing"
	"time"
)

func TestIdentifyBackoffDoublesSpacingAndCaps(t *testing.T) {
	b := newIdentifyBackoff()
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
	b := newIdentifyBackoff()
	now := time.Unix(0, 0)
	for i := 0; i < 8; i++ {
		now = b.reserve(now)
	}

	later := now.Add(identifyStableAfter)
	if got := b.reserve(later); !got.Equal(later) {
		t.Fatalf("IDENTIFY after a stable stretch must be immediate, got a wait of %v", got.Sub(later))
	}
}
