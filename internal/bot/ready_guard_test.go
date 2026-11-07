package bot

import (
	"testing"
	"time"
)

func TestReadyGuardAllowsOccasionalReady(t *testing.T) {
	now := time.Unix(0, 0)
	g := newReadyGuard()
	g.now = func() time.Time { return now }

	for i := 0; i < 20; i++ {
		now = now.Add(readyStormWindow)
		if allowed, tripped := g.admit(); !allowed || tripped {
			t.Fatalf("ready %d spaced a window apart must be allowed, got allowed=%v tripped=%v", i, allowed, tripped)
		}
	}
}

func TestReadyGuardBlocksStormAndReportsItOnce(t *testing.T) {
	now := time.Unix(0, 0)
	g := newReadyGuard()
	g.now = func() time.Time { return now }

	trips := 0
	for i := 0; i < 200; i++ {
		now = now.Add(5 * time.Second)
		allowed, tripped := g.admit()
		if i < readyStormLimit && !allowed {
			t.Fatalf("ready %d is within the limit and must be allowed", i)
		}
		if i >= readyStormLimit && allowed {
			t.Fatalf("ready %d arrives every 5s and must be blocked", i)
		}
		if tripped {
			trips++
		}
	}
	if trips != 1 {
		t.Fatalf("storm must be reported once, got %d", trips)
	}
}

func TestReadyGuardRecoversAfterStorm(t *testing.T) {
	now := time.Unix(0, 0)
	g := newReadyGuard()
	g.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		now = now.Add(5 * time.Second)
		g.admit()
	}
	now = now.Add(readyStormWindow + time.Second)
	if allowed, _ := g.admit(); !allowed {
		t.Fatal("a ready after a quiet window must seed again")
	}
}
