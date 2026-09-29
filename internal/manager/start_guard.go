package manager

import (
	"context"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// startGuard admits one raid start per guild at a time. The zero value is
// ready to use.
//
// The slash command checks for an active session before dispatching, but the
// start it dispatches runs asynchronously and takes as long as every bot needs
// to join, so a second /start lands inside that window with nothing to see.
// commitSession re-checks under m.mu, which keeps two sessions from ever being
// committed — but by then both starts have joined every bot, and none of that
// work is session-scoped: the voice conns belong to the bot and guild, so the
// second start overwrites the first's provider and receiver on the very same
// conn, and the loser's cleanup then leaves the channels, closes those conns
// and unregisters the guild from the ally registry (both starts create an ally
// session under the guild's one persistent relay code). The winner survives in
// m.statuses with no bots in voice and a relay code no guest can join.
//
// So the second start has to be rejected before it touches anything, which is
// what this guard does. It also holds each start's cancel func, so /stop can
// abort a raid that is still coming up rather than report that there is
// nothing to stop.
type startGuard struct {
	mu       sync.Mutex
	inFlight map[snowflake.ID]context.CancelFunc
}

// tryBegin reserves guildID for one start. When it returns false a start is
// already running for that guild and the caller must do nothing; when it
// returns true the caller must call end before returning.
func (g *startGuard) tryBegin(guildID snowflake.ID, cancel context.CancelFunc) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.inFlight[guildID]; ok {
		return false
	}
	if g.inFlight == nil {
		g.inFlight = make(map[snowflake.ID]context.CancelFunc)
	}
	g.inFlight[guildID] = cancel
	return true
}

// cancel aborts the start in flight for guildID and reports whether there was
// one. The reservation outlives the cancel on purpose: the start is still
// unwinding what it joined, and a fresh start let in during that would race it
// exactly as two starts did.
func (g *startGuard) cancel(guildID snowflake.ID) bool {
	g.mu.Lock()
	cancel, ok := g.inFlight[guildID]
	g.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
	return ok
}

// end releases the reservation tryBegin took.
func (g *startGuard) end(guildID snowflake.ID) {
	g.mu.Lock()
	delete(g.inFlight, guildID)
	g.mu.Unlock()
}
