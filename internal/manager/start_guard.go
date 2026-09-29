package manager

import (
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
// what this guard does.
type startGuard struct {
	mu       sync.Mutex
	inFlight map[snowflake.ID]struct{}
}

// tryBegin reserves guildID for one start. When it returns false a start is
// already running for that guild and the caller must do nothing; when it
// returns true the caller must call end before returning.
func (g *startGuard) tryBegin(guildID snowflake.ID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.inFlight[guildID]; ok {
		return false
	}
	if g.inFlight == nil {
		g.inFlight = make(map[snowflake.ID]struct{})
	}
	g.inFlight[guildID] = struct{}{}
	return true
}

// end releases the reservation tryBegin took.
func (g *startGuard) end(guildID snowflake.ID) {
	g.mu.Lock()
	delete(g.inFlight, guildID)
	g.mu.Unlock()
}
