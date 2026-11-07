package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	identifyBackoffBase = time.Second
	identifyBackoffMax  = 5 * time.Minute
	identifyStableAfter = 10 * time.Minute
)

// identifyBackoff spaces out IDENTIFYs while a gateway keeps needing them.
// disgo restarts its reconnect backoff after every READY, so a gateway that is
// ready but keeps being invalidated would otherwise be re-identified every
// second and spend Discord's 1,000 IDENTIFYs a day, which resets the token.
type identifyBackoff struct {
	mu     sync.Mutex
	now    func() time.Time
	last   time.Time
	streak int
}

func newIdentifyBackoff() *identifyBackoff {
	return &identifyBackoff{now: time.Now}
}

func (b *identifyBackoff) Wait(ctx context.Context, _ int) error {
	b.mu.Lock()
	now := b.now()
	wait := b.reserve(now).Sub(now)
	b.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	slog.Warn("gateway is being re-identified; delaying the next IDENTIFY",
		slog.Duration("delay", wait),
	)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *identifyBackoff) Unlock(int) {}

func (b *identifyBackoff) Close(context.Context) {}

// reserve books the next IDENTIFY slot: the first after a stable stretch is
// immediate, each consecutive one waits twice as long as the one before.
func (b *identifyBackoff) reserve(now time.Time) time.Time {
	if !b.last.IsZero() && now.Sub(b.last) >= identifyStableAfter {
		b.streak = 0
	}

	slot := now
	if b.streak > 0 {
		spacing := identifyBackoffBase
		for i := 1; i < b.streak && spacing < identifyBackoffMax; i++ {
			spacing *= 2
		}
		if earliest := b.last.Add(min(spacing, identifyBackoffMax)); earliest.After(slot) {
			slot = earliest
		}
	}

	b.last = slot
	b.streak++
	return slot
}
