package bot

import (
	"sync"
	"time"
)

const (
	readyStormWindow = 10 * time.Minute
	readyStormLimit  = 5
)

// readyGuard detects a gateway that keeps re-identifying. Every READY re-seeds
// all guilds over REST, so a reconnect loop turns into a REST flood that keeps
// Discord's global rate limit tripped and the loop going.
type readyGuard struct {
	mu     sync.Mutex
	now    func() time.Time
	window time.Duration
	limit  int
	seen   []time.Time
	storm  bool
}

func newReadyGuard() *readyGuard {
	return &readyGuard{now: time.Now, window: readyStormWindow, limit: readyStormLimit}
}

// admit records a READY and reports whether it should trigger seeding. tripped
// is true only on the READY that starts a storm, so it is logged once.
func (g *readyGuard) admit() (allowed, tripped bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	cutoff := now.Add(-g.window)
	kept := g.seen[:0]
	for _, t := range g.seen {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.seen = append(kept, now)

	if len(g.seen) <= g.limit {
		g.storm = false
		return true, false
	}
	tripped = !g.storm
	g.storm = true
	return false, tripped
}
