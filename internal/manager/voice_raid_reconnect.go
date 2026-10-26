package manager

import (
	"context"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// reconnectApplier re-applies voice provider/receiver to a freshly opened conn
// after a bot reconnects to its bound channel mid-session. It creates new
// provider/receiver objects from the same channels so the mixer graph stays connected.
// ctx is the reconnect context (not the original session context) so that metric
// recording and speaking-flag ops use a live, uncancelled context.
type reconnectApplier func(ctx context.Context, conn voice.Conn)

// botKey is a composite key for the reconnect state maps.
// Using a struct avoids the three string allocations (two .String() calls plus
// concatenation) that occurred on every GuildVoiceLeave event with string keys.
type botKey struct {
	guildID   snowflake.ID
	botUserID snowflake.ID
}

// reconnectRegistry is a typed, mutex-guarded map of reconnect appliers.
// It replaces sync.Map to eliminate type assertions on every load and to
// provide a clear, self-documenting API for the reconnect subsystem.
type reconnectRegistry struct {
	mu sync.RWMutex
	m  map[botKey]reconnectApplier
}

func newReconnectRegistry() reconnectRegistry {
	return reconnectRegistry{m: make(map[botKey]reconnectApplier)}
}

func (r *reconnectRegistry) store(k botKey, a reconnectApplier) {
	r.mu.Lock()
	r.m[k] = a
	r.mu.Unlock()
}

func (r *reconnectRegistry) load(k botKey) (reconnectApplier, bool) {
	r.mu.RLock()
	a, ok := r.m[k]
	r.mu.RUnlock()
	return a, ok
}

func (r *reconnectRegistry) delete(k botKey) {
	r.mu.Lock()
	delete(r.m, k)
	r.mu.Unlock()
}

// inFlightSet tracks in-progress reconnect attempts. It uses sync.Map because
// the key operation — LoadOrStore — requires atomic check-and-set semantics
// that a plain mutex map cannot express without a write-lock for every read.
type inFlightSet struct {
	m sync.Map // botKey → struct{}
}

// trySet marks key as in-flight. Returns true if the caller successfully
// claimed the slot (i.e. no prior attempt was running), false if already set.
func (s *inFlightSet) trySet(k botKey) bool {
	_, loaded := s.m.LoadOrStore(k, struct{}{})
	return !loaded
}

func (s *inFlightSet) unset(k botKey) {
	s.m.Delete(k)
}

// stallRejoinBackoffMin is the wait before a bot that stalled again is
// rejoined a second time; each further rejoin doubles it up to
// stallRejoinBackoffMax. A stall that survives a rejoin usually means the new
// DAVE session is just as broken, and rejoining every stall report (~10 s)
// only churns the voice gateway.
const (
	stallRejoinBackoffMin = 30 * time.Second
	stallRejoinBackoffMax = 5 * time.Minute
)

type stallRejoin struct {
	lastReport time.Time
	nextRejoin time.Time
	delay      time.Duration
}

// stallBackoff rate-limits encrypt-stall rejoins per (guild, bot). A bot that
// has gone stallRejoinBackoffMax without a stall report starts over.
type stallBackoff struct {
	mu sync.Mutex
	m  map[botKey]stallRejoin
}

func newStallBackoff() stallBackoff {
	return stallBackoff{m: make(map[botKey]stallRejoin)}
}

// due records a stall report at now and reports whether it should rejoin.
func (b *stallBackoff) due(k botKey, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, seen := b.m[k]
	if !seen || now.Sub(r.lastReport) >= stallRejoinBackoffMax {
		r = stallRejoin{delay: stallRejoinBackoffMin}
	}
	r.lastReport = now
	due := !now.Before(r.nextRejoin)
	if due {
		r.nextRejoin = now.Add(r.delay)
		r.delay = min(2*r.delay, stallRejoinBackoffMax)
	}
	b.m[k] = r
	return due
}

func (b *stallBackoff) delete(k botKey) {
	b.mu.Lock()
	delete(b.m, k)
	b.mu.Unlock()
}

// reconnectState bundles the reconnect subsystem maps and their operations.
// Extracting them from Service clarifies ownership and eliminates type assertions.
type reconnectState struct {
	appliers reconnectRegistry
	inFlight inFlightSet
	stalls   stallBackoff
}

func newReconnectState() reconnectState {
	return reconnectState{appliers: newReconnectRegistry(), stalls: newStallBackoff()}
}

func (rs *reconnectState) stallRejoinDue(guildID, botUserID snowflake.ID, now time.Time) bool {
	return rs.stalls.due(botKey{guildID, botUserID}, now)
}

// tryLock claims the in-flight slot for the given (guild, bot) pair.
// Returns true on success; false means a reconnect is already running.
func (rs *reconnectState) tryLock(guildID, botUserID snowflake.ID) bool {
	return rs.inFlight.trySet(botKey{guildID, botUserID})
}

func (rs *reconnectState) unlock(guildID, botUserID snowflake.ID) {
	rs.inFlight.unset(botKey{guildID, botUserID})
}

func (rs *reconnectState) storeApplier(guildID, botUserID snowflake.ID, a reconnectApplier) {
	rs.appliers.store(botKey{guildID, botUserID}, a)
}

func (rs *reconnectState) loadApplier(guildID, botUserID snowflake.ID) (reconnectApplier, bool) {
	return rs.appliers.load(botKey{guildID, botUserID})
}

// clearGuild removes all appliers and stall backoff for guildID using the known
// bot ID set, avoiding a full O(N*M) map scan.
func (rs *reconnectState) clearGuild(guildID snowflake.ID, botIDs []snowflake.ID, ownerBotID snowflake.ID) {
	for _, botID := range botIDs {
		rs.clearBot(botKey{guildID, botID})
	}
	rs.clearBot(botKey{guildID, ownerBotID})
}

func (rs *reconnectState) clearBot(k botKey) {
	rs.appliers.delete(k)
	rs.stalls.delete(k)
}
