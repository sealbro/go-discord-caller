package manager

import (
	"context"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
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

// reconnectState bundles the two reconnect subsystem maps and their operations.
// Extracting them from Service clarifies ownership and eliminates type assertions.
type reconnectState struct {
	appliers reconnectRegistry
	inFlight inFlightSet
}

func newReconnectState() reconnectState {
	return reconnectState{appliers: newReconnectRegistry()}
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

// clearAppliers removes all appliers for guildID using the known bot ID set,
// avoiding a full O(N*M) map scan.
func (rs *reconnectState) clearAppliers(guildID snowflake.ID, botIDs []snowflake.ID, ownerBotID snowflake.ID) {
	for _, botID := range botIDs {
		rs.appliers.delete(botKey{guildID, botID})
	}
	rs.appliers.delete(botKey{guildID, ownerBotID})
}

// ChannelAccessWarning describes a bot that cannot connect or speak in its bound channel.
type ChannelAccessWarning struct {
	BotID     snowflake.ID
	ChannelID snowflake.ID
}

// voiceLeaveTimeout is the maximum time to wait for a voice Leave call.
// Using context.Background() without a deadline risks hanging forever if Discord
// is unresponsive during session teardown.
const voiceLeaveTimeout = 5 * time.Second

// relayInputID is the synthetic source ID used when adding a guest relay feed
// as an input to a host-side ChannelMixer. Discord snowflakes are epoch-based
// (minimum value ~4 billion) so 1 never collides with a real user/bot ID.
const relayInputID snowflake.ID = 1

// sourceEntry is one audio capture channel feeding the relay mixer graph.
// handle is non-nil when the source's VoiceReceiver dispatches via FanoutHandle
// (modern fanout mode). The wiring code calls handle.Install to attach mixer
// inputs; handle.Close at session-end fires the install-time OnClose hook.
type sourceEntry struct {
	id        snowflake.ID
	channelID snowflake.ID
	ch        <-chan []byte
	handle    *opus.FanoutHandle
}

// destChannel groups all speaker output channels that share the same voice channel.
type destChannel struct {
	channelID snowflake.ID
	outs      []chan<- []byte
}

// speakerResult holds the outcome of a single successfully joined speaker.
type speakerResult struct {
	speaker   guild.Speaker
	chOut     chan<- []byte
	chCapture <-chan []byte      // nil when withCapture is false
	handle    *opus.FanoutHandle // nil when withCapture is false
	gv        pool.GuildVoice
	cleanup   func() // closes provider/receiver; caller must invoke on teardown
}

// raidSetup captures the common setup result for both host and guest flows.
type raidSetup struct {
	joined         []speakerResult
	speakers       []guild.Speaker
	speakerCleanup func()
	outs           []chan<- []byte
}
