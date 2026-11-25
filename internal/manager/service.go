package manager

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/raid"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric"
)

// Service orchestrates speaker bots and voice raid sessions.
// It is the sole owner of all GuildStatus state; callers receive safe value copies.
type Service struct {
	mu       sync.RWMutex
	statuses map[snowflake.ID]*guild.Status // protected by mu

	// activeRouters mirrors the subset of statuses whose Session has a
	// non-nil AutoRouter, kept as an immutable map snapshot behind an
	// atomic.Pointer. Voice events go through AutoRoute on every Discord
	// event — looking up the router here avoids taking m.mu on the hot
	// path. Updated under m.mu by setActiveRouter / clearActiveRouter
	// whenever a session is committed or torn down.
	activeRouters atomic.Pointer[map[snowflake.ID]guild.AutoRouter]

	store              store.Store
	poolSvc            pool.PoolService
	ownerClient        *bot.Client
	ownerBotID         snowflake.ID
	voices             voiceHost    // nil runs voice on the bots' own clients; see voiceManagers
	audio              raid.Runtime // nil builds raid audio in this process; see raidRuntime
	views              sync.Map     // guild ID → *viewProbe of the guild's raid; see RefreshVoiceView
	bindMu             sync.Mutex
	toggleMu           sync.Mutex // orders speaker toggles so the store write follows the in-memory change
	test               config.TestConfig
	sessions           *ally.Manager
	metrics            *telemetry.Metrics
	reconnect          reconnectState // typed reconnect subsystem (applier registry + in-flight guard)
	rejoinStalled      func(guildID, botUserID snowflake.ID)
	starting           startGuard    // one raid start per guild at a time
	sessionIdleTimeout time.Duration // 0 disables; set via SetSessionIdleTimeout
	reportedCallers    reportedCallers
	seeding            seedGroup
}

type voiceChannelKey struct{ guildID, channelID snowflake.ID }

// reportedCallers remembers which channels the last voice-callers collection
// exported, so a channel that empties is reported as 0 once rather than
// dropped: the backend carries a dropped series' last value forward for five
// minutes and keeps counting callers who already left.
type reportedCallers struct {
	mu       sync.Mutex
	channels map[voiceChannelKey]struct{}
}

// NewService creates a new manager Service.
func NewService(st store.Store, poolSvc pool.PoolService, ownerClient *bot.Client, ownerID snowflake.ID, test config.TestConfig, metrics *telemetry.Metrics) *Service {
	s := &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       st,
		poolSvc:     poolSvc,
		ownerClient: ownerClient,
		ownerBotID:  ownerID,
		test:        test,
		sessions:    ally.NewManager(),
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}
	s.rejoinStalled = func(guildID, botUserID snowflake.ID) {
		go s.ReconnectBotChannel(context.Background(), guildID, botUserID)
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	s.activeRouters.Store(&empty)
	return s
}

// setActiveRouter publishes a session's AutoRouter into the lock-free
// activeRouters snapshot. Called from commitSession + JoinSession under
// m.mu so the published snapshot reflects the freshly-committed session.
// Copy-on-write: replaces the entire map so AutoRoute readers always see
// an immutable snapshot.
func (m *Service) setActiveRouter(guildID snowflake.ID, r guild.AutoRouter) {
	if r == nil {
		m.clearActiveRouter(guildID)
		return
	}
	cur := *m.activeRouters.Load()
	next := make(map[snowflake.ID]guild.AutoRouter, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[guildID] = r
	m.activeRouters.Store(&next)
}

// clearActiveRouter removes a guild from the activeRouters snapshot and stops
// refreshing its view of voice. Called from StopVoiceRaid / JoinSession-teardown
// under m.mu.
func (m *Service) clearActiveRouter(guildID snowflake.ID) {
	m.closeGuildView(guildID)
	cur := *m.activeRouters.Load()
	if _, present := cur[guildID]; !present {
		return
	}
	next := make(map[snowflake.ID]guild.AutoRouter, len(cur)-1)
	for k, v := range cur {
		if k == guildID {
			continue
		}
		next[k] = v
	}
	m.activeRouters.Store(&next)
}

// SetSessionIdleTimeout configures the duration after which a session with no
// caller speaking to a listener in a speaker channel is auto-stopped.
// Pass 0 to disable. Call once at startup, before any session is started.
func (m *Service) SetSessionIdleTimeout(d time.Duration) {
	m.sessionIdleTimeout = d
}

// startSessionIdleWatcher launches a goroutine that stops the session once it
// has had no caller speaking to a listener for m.sessionIdleTimeout. Safe to call for any session — it no-ops when the
// timeout is disabled or the session has no bound channels to watch.
func (m *Service) startSessionIdleWatcher(ctx context.Context, cancelFunc context.CancelFunc, session *guild.Session) {
	if m.sessionIdleTimeout <= 0 {
		return
	}
	channels, speakerChannels := m.sessionChannels(session)
	if len(channels) == 0 {
		return
	}
	w := &sessionPresenceWatcher{
		guildID:          session.GuildID,
		channels:         channels,
		listenerChannels: speakerChannels,
		probe:            &cacheVoiceProbe{svc: m, guildID: session.GuildID},
		callerRole:       func() snowflake.ID { return m.callerRoleID(session.GuildID) },
		stop:             func() { m.stopIdleSession(session, cancelFunc) },
		idleTimeout:      m.sessionIdleTimeout,
		sharesRelay:      func() bool { return m.sharesRelay(session.GuildID) },
	}
	if session.AllowFilter != nil {
		w.lastHeard = session.AllowFilter.LastAdmitted
	}
	go w.Run(ctx)
}

// sharesRelay reports whether guildID's audio is shared with another server:
// it hosts a relay with guests attached, or is a guest of one.
func (m *Service) sharesRelay(guildID snowflake.ID) bool {
	relay, ok := m.sessions.GetByGuild(guildID)
	if !ok {
		return false
	}
	return relay.HostGuildID != guildID || len(relay.GuestGuildIDs()) > 0
}

func (m *Service) callerRoleID(guildID snowflake.ID) snowflake.ID {
	id, _ := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	return id
}

// stopIdleSession runs the same teardown as /stop so the owner's voice conn is
// released, and only for the session that went idle. cancelFunc is the fallback
// when that session is already gone: its goroutine still needs the context closed.
func (m *Service) stopIdleSession(session *guild.Session, cancelFunc context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), idleStopTimeout)
	defer cancel()
	if err := m.stopSession(ctx, session.GuildID, session); err != nil {
		slog.WarnContext(ctx, "session idle: stop skipped, cancelling session context",
			slog.String("guildID", session.GuildID.String()), slog.Any("err", err))
		cancelFunc()
	}
}

// sessionChannels returns the deduplicated set of voice channels to watch for a
// session — the owner bot's bound channel plus every session speaker's bound
// channel — and, among them, the ones a bot plays audio into: every speaker's,
// and the owner's in capture modes, where it plays the other channels back.
func (m *Service) sessionChannels(session *guild.Session) (channels, listenerChannels []snowflake.ID) {
	guildID := session.GuildID
	seen := make(map[snowflake.ID]struct{})
	add := func(id snowflake.ID, ok, playedInto bool) {
		if !ok || id == 0 {
			return
		}
		if playedInto && !slices.Contains(listenerChannels, id) {
			listenerChannels = append(listenerChannels, id)
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		channels = append(channels, id)
	}
	ownerCh, ok := m.store.GetBoundChannel(guildID, m.ownerBotID)
	add(ownerCh, ok, session.Mode.WithCapture())
	for _, sp := range session.Speakers {
		chID, ok := m.store.GetBoundChannel(guildID, sp.ID)
		add(chID, ok, true)
	}
	return channels, listenerChannels
}

// StartMetrics registers OTel observable metric callbacks.
// Call once after the speaker pool is connected, alongside StartWatchdog.
func (m *Service) StartMetrics() {
	if err := m.metrics.Bot.RegisterGuilds(m.observeGuilds); err != nil {
		slog.Error("manager: failed to register guild metric callback", slog.Any("err", err))
	}
	if err := m.metrics.Bot.RegisterVoiceCallers(m.observeVoiceCallers); err != nil {
		slog.Error("manager: failed to register voice_callers metric callback", slog.Any("err", err))
	}
}

// observeGuilds is an OTel observable callback that emits gdc_discord_guild for
// every guild the owner bot is currently a member of, valued with how many bots
// (the owner and its registered speakers) it has there. It is one series per
// guild, not per bot, so cardinality does not grow with the pool.
func (m *Service) observeGuilds(ctx context.Context, o metric.Observer) error {
	speakers := m.registeredSpeakerCounts()
	for g := range m.ownerClient.Caches.Guilds() {
		m.metrics.Bot.ObserveGuild(o, g.ID.String(), g.Name, 1+speakers[g.ID])
		m.metrics.Session.PrimeGuild(ctx, g.ID)
	}
	return nil
}

// registeredSpeakerCounts counts, per guild, the pool bots registered as its
// speakers. Taken under the read lock so that metric emission, which calls into
// the OTel SDK, does not hold m.mu and block voice raid writes.
func (m *Service) registeredSpeakerCounts() map[snowflake.ID]int {
	poolIDs := m.poolSvc.GetIDs()
	m.mu.RLock()
	defer m.mu.RUnlock()
	counts := make(map[snowflake.ID]int, len(m.statuses))
	for guildID, st := range m.statuses {
		for _, botID := range poolIDs {
			if _, ok := st.Speakers[botID]; ok {
				counts[guildID]++
			}
		}
	}
	return counts
}

// observeVoiceCallers is an OTel observable callback that emits gdc_voice_callers
// per voice channel for guilds with an active raid, recounted from the owner
// bot's cache on every collection.
// A running count kept from join/leave events drifts for good on any event it
// misses — callers already in voice at startup, a role granted or revoked
// mid-call — while the cache stays correct by construction.
func (m *Service) observeVoiceCallers(_ context.Context, o metric.Observer) error {
	counts := m.countVoiceCallers()

	m.reportedCallers.mu.Lock()
	defer m.reportedCallers.mu.Unlock()
	for ch := range m.reportedCallers.channels {
		if _, stillReported := counts[ch]; !stillReported {
			m.metrics.Bot.ObserveVoiceCallers(o, 0, ch.guildID.String(), ch.channelID.String())
		}
	}
	reported := make(map[voiceChannelKey]struct{}, len(counts))
	for ch, count := range counts {
		m.metrics.Bot.ObserveVoiceCallers(o, count, ch.guildID.String(), ch.channelID.String())
		reported[ch] = struct{}{}
	}
	m.reportedCallers.channels = reported
	return nil
}

func (m *Service) countVoiceCallers() map[voiceChannelKey]int64 {
	caches := m.ownerClient.Caches
	counts := make(map[voiceChannelKey]int64)
	for g := range caches.Guilds() {
		captured := m.capturedChannels(g.ID)
		if len(captured) == 0 {
			continue
		}
		for vs := range caches.VoiceStates(g.ID) {
			if vs.ChannelID == nil {
				continue
			}
			if _, ok := captured[*vs.ChannelID]; !ok {
				continue
			}
			member, ok := caches.Member(g.ID, vs.UserID)
			if !ok || m.IsBot(member.User) || !m.HasCallerRole(g.ID, member.RoleIDs) {
				continue
			}
			counts[voiceChannelKey{g.ID, *vs.ChannelID}]++
		}
	}
	return counts
}

// capturedChannels returns the voice channels whose audio the guild's active
// raid relays: the owner's on a host raid or a guest raid that captures, and
// the speakers' in the modes where speakers capture. Empty without a raid.
func (m *Service) capturedChannels(guildID snowflake.ID) map[snowflake.ID]struct{} {
	m.mu.RLock()
	var session guild.Session
	if st := m.statuses[guildID]; st != nil && st.HasActiveSession() {
		session = *st.Session
	}
	m.mu.RUnlock()
	if session.GuildID == 0 {
		return nil
	}

	channels := make(map[snowflake.ID]struct{})
	add := func(botID snowflake.ID) {
		if ch, ok := m.store.GetBoundChannel(guildID, botID); ok {
			channels[ch] = struct{}{}
		}
	}
	if !session.IsGuest || session.Mode.WithCapture() {
		add(m.ownerBotID)
	}
	if session.Mode.WithCapture() {
		for _, sp := range session.Speakers {
			add(sp.ID)
		}
	}
	return channels
}

// ownerVoice returns a GuildVoice for the owner bot in guildID, bound to its
// configured channel, or to none when that channel no longer exists. Use
// Join/Leave on the result to manage the connection.
func (m *Service) ownerVoice(guildID snowflake.ID) pool.GuildVoice {
	channelID, _ := m.liveBoundChannel(guildID, m.ownerBotID)
	vm, _ := m.voiceManagers().VoiceManager(m.ownerBotID)
	return pool.NewGuildVoice(vm, channelID).ForBot(m.ownerBotID)
}

func (m *Service) seedGuildSpeakers(guildID, ownerID snowflake.ID) {
	type initSpeaker struct {
		id      snowflake.ID
		speaker *guild.Speaker
		err     error
	}
	var speakers []initSpeaker
	for _, botUserID := range m.poolSvc.GetIDs() {
		if !m.isGuildMember(guildID, botUserID) {
			continue
		}
		newSpeaker, err := m.newSpeaker(guildID, botUserID)
		speakers = append(speakers, initSpeaker{botUserID, newSpeaker, err})
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.statuses[guildID]
	if !ok {
		st = guild.NewStatus(guildID, ownerID)
		m.statuses[guildID] = st
	}
	for _, init := range speakers {
		if init.err != nil {
			slog.Warn("seed: failed to register existing speaker bot",
				slog.String("botUserID", init.id.String()),
				slog.String("guildID", guildID.String()),
				slog.Any("err", init.err),
			)
			continue
		}
		if _, exists := st.Speakers[init.speaker.ID]; !exists {
			st.Speakers[init.speaker.ID] = init.speaker
			slog.Info("seed: registered existing speaker bot",
				slog.String("username", init.speaker.Username),
				slog.String("guildID", guildID.String()),
			)
		}
	}
}

// Shutdown stops every active voice raid and closes all speaker gateways.
func (m *Service) Shutdown(ctx context.Context) {
	slog.InfoContext(ctx, "shutting down manager service...")

	// Collect active guild IDs under read lock to avoid holding it during I/O.
	m.mu.RLock()
	activeGuilds := make([]snowflake.ID, 0, len(m.statuses))
	for guildID, st := range m.statuses {
		if st.HasActiveSession() {
			activeGuilds = append(activeGuilds, guildID)
		}
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, guildID := range activeGuilds {
		wg.Go(func() {
			if err := m.StopVoiceRaid(ctx, guildID); err != nil {
				slog.WarnContext(ctx, "shutdown: failed to stop voice raid",
					slog.String("guildID", guildID.String()),
					slog.Any("err", err),
				)
			}
		})
	}
	wg.Wait()
	m.poolSvc.Shutdown(ctx)
}

// IsBot reports whether user should be treated as a bot for filtering purposes.
// Returns false in E2E test mode (AllowBots=true) so test bot accounts pass
// the same checks that human users pass.
func (m *Service) IsBot(user discord.User) bool {
	return user.Bot && !m.test.AllowBots
}

// cacheVoiceProbe is the production CallerEnumerator used by the auto-router.
// It reads the owner bot's voice-state and member caches to enumerate
// role-bearing users currently in a channel.
//
// Held by the router for the session lifetime. The guildID is captured at
// construction so the router only needs to pass channelID + roleID on each
// query.
type cacheVoiceProbe struct {
	svc     *Service
	guildID snowflake.ID
}

// HasListeners reports whether channelID currently contains at least one
// non-bot member according to the owner bot's voice-state cache. Used by the
// router to pause destination mixers whose voice channel has no humans to
// hear the audio.
func (p *cacheVoiceProbe) HasListeners(channelID snowflake.ID) bool {
	return len(p.Listeners(channelID)) > 0
}

// Listeners returns the user IDs of the non-bot members the owner bot's caches
// place in channelID.
func (p *cacheVoiceProbe) Listeners(channelID snowflake.ID) []snowflake.ID {
	caches := p.svc.ownerClient.Caches
	var users []snowflake.ID
	for vs := range caches.VoiceStates(p.guildID) {
		if vs.ChannelID == nil || *vs.ChannelID != channelID {
			continue
		}
		member, ok := caches.Member(p.guildID, vs.UserID)
		if ok && !p.svc.IsBot(member.User) {
			users = append(users, vs.UserID)
		}
	}
	return users
}

// EnumerateCallers returns the user IDs of cached non-bot members in
// channelID whose role set contains roleID. When roleID == 0 (no caller role
// bound) nobody counts — matching AllowFilter semantics.
func (p *cacheVoiceProbe) EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID {
	return p.callers(channelID, roleID, false)
}

// EnumerateUnmutedCallers is EnumerateCallers without the members who are muted,
// by themselves or by the server: a muted caller sends nothing to relay.
func (p *cacheVoiceProbe) EnumerateUnmutedCallers(channelID, roleID snowflake.ID) []snowflake.ID {
	return p.callers(channelID, roleID, true)
}

func (p *cacheVoiceProbe) callers(channelID, roleID snowflake.ID, skipMuted bool) []snowflake.ID {
	caches := p.svc.ownerClient.Caches
	var users []snowflake.ID
	for vs := range caches.VoiceStates(p.guildID) {
		if vs.ChannelID == nil || *vs.ChannelID != channelID {
			continue
		}
		if skipMuted && (vs.SelfMute || vs.GuildMute) {
			continue
		}
		member, ok := caches.Member(p.guildID, vs.UserID)
		if !ok || p.svc.IsBot(member.User) {
			continue
		}
		for _, rID := range member.RoleIDs {
			if rID == roleID {
				users = append(users, vs.UserID)
				break
			}
		}
	}
	return users
}

// AutoRoute notifies the auto-router that a voice state event has touched
// channelID. The router debounces bursts (250 ms by default) before
// recomputing per-source modes AND per-destination pause state (cascade and
// listener check folded together — Plan §3.6). Safe to call when there is no
// active session or no router attached to the session — both paths are no-ops.
func (m *Service) AutoRoute(guildID, channelID snowflake.ID) {
	// Lock-free hot path: voice events fire on every Discord state change
	// (typically many per second per active guild). Reading the
	// activeRouters atomic.Pointer avoids m.mu contention with the slower
	// raid-lifecycle writers.
	if routers := m.activeRouters.Load(); routers != nil {
		if router, ok := (*routers)[guildID]; ok {
			router.Debounce(channelID)
		}
	}
}

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
	// Fast path: member cache is pre-warmed by warmGuildCache at startup.
	if _, ok := m.ownerClient.Caches.Member(guildID, userID); ok {
		return true
	}
	// Slow path: cache miss — fall back to REST (e.g. first call before warmup).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.ownerClient.Rest.GetMember(guildID, userID, rest.WithCtx(ctx))
	return err == nil
}

// snapshotLocked returns a deep copy of st enriched with live channel/role data.
// Must be called with mu read-locked (store calls are safe; store has its own lock).
func (m *Service) snapshotLocked(guildID snowflake.ID) guild.Status {
	st, ok := m.statuses[guildID]
	if !ok {
		return guild.Status{
			GuildID:       guildID,
			OwnerUserID:   m.ownerBotID,
			Speakers:      make(map[snowflake.ID]*guild.Speaker),
			BoundChannels: make(map[snowflake.ID]snowflake.ID),
		}
	}

	// Deep-copy the struct so callers cannot race with future mutations.
	snap := *st
	snap.Speakers = make(map[snowflake.ID]*guild.Speaker, len(st.Speakers))
	for k, v := range st.Speakers {
		snap.Speakers[k] = new(*v)
	}
	snap.BoundChannels = make(map[snowflake.ID]snowflake.ID, len(st.BoundChannels))
	maps.Copy(snap.BoundChannels, st.BoundChannels)
	// Deep-copy the session so the snapshot holds a fully independent copy.
	// Nil out Cancel/Cleanup — snapshots are read-only display objects.
	if st.Session != nil {
		sessionCopy := *st.Session
		sessionCopy.Cancel = nil
		sessionCopy.Cleanup = nil
		sessionCopy.ChannelMixers = nil
		sessionCopy.AutoRouter = nil
		sessionCopy.Speakers = make([]guild.Speaker, len(st.Session.Speakers))
		copy(sessionCopy.Speakers, st.Session.Speakers)
		snap.Session = &sessionCopy
	}
	return snap
}

// speakerVoice returns a GuildVoice for a speaker bot in guildID, bound to its
// configured channel. Use Join/Leave on the result to manage the connection.
// Returns false if the bot is not in the pool or its gateway is not connected.
func (m *Service) speakerVoice(guildID, botUserID snowflake.ID) (pool.GuildVoice, bool) {
	vm, ok := m.voiceManagers().VoiceManager(botUserID)
	if !ok {
		return pool.GuildVoice{}, false
	}
	channelID, _ := m.store.GetBoundChannel(guildID, botUserID)
	return pool.NewGuildVoice(vm, channelID).ForBot(botUserID), true
}

// CheckGuildChannelAccess checks Connect+Speak permissions for the owner bot and
// all enabled, bound speaker bots in guildID. Returns one ChannelAccessWarning for
// each bot whose permissions are definitively denied. Relies on the cache pre-warmed
// by warmGuildCache at startup; bots or channels not yet cached are skipped silently.
// In test mode (AllowBots=true) the check is skipped entirely — test bot accounts
// may not have full Discord permissions configured.
func (m *Service) CheckGuildChannelAccess(guildID snowflake.ID) []ChannelAccessWarning {
	if m.test.AllowBots {
		return nil
	}
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return nil
	}

	var warnings []ChannelAccessWarning

	// Owner bot.
	if ownerChID, ok := m.store.GetBoundChannel(guildID, m.ownerBotID); ok {
		if w, denied := m.botChannelWarning(m.ownerBotID, guildID, ownerChID); denied {
			warnings = append(warnings, w)
		}
	}

	// Speaker bots.
	for _, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		chID, ok := m.store.GetBoundChannel(guildID, sp.ID)
		if !ok {
			continue
		}
		if w, denied := m.botChannelWarning(sp.ID, guildID, chID); denied {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// warmGuildCache fetches channels and bot members for guildID via REST and populates
// the owner bot's cache. Call once per guild at startup so that CheckGuildChannelAccess
// can be fully cache-based without per-call REST lookups.
func (m *Service) warmGuildCache(guildID snowflake.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	channels, err := m.ownerClient.Rest.GetGuildChannels(guildID, rest.WithCtx(ctx))
	if err != nil {
		slog.Warn("warmGuildCache: failed to fetch guild channels",
			slog.String("guildID", guildID.String()),
			slog.Any("err", err),
		)
	} else {
		existing := make(map[snowflake.ID]struct{}, len(channels))
		for _, ch := range channels {
			m.ownerClient.Caches.AddChannel(ch)
			existing[ch.ID()] = struct{}{}
		}
		// The fetched list is authoritative, so a binding missing from it is
		// stale. A failed fetch proves nothing and leaves bindings alone.
		m.unbindChannels(guildID, func(channelID snowflake.ID) bool {
			_, ok := existing[channelID]
			return !ok
		})
		slog.Debug("warmGuildCache: populated channel cache",
			slog.String("guildID", guildID.String()),
			slog.Int("count", len(channels)),
		)
	}

	// Warm member cache for owner bot and all pool speaker bots in this guild.
	botIDs := append([]snowflake.ID{m.ownerBotID}, m.poolSvc.GetIDs()...)
	for _, botID := range botIDs {
		if _, ok := m.ownerClient.Caches.Member(guildID, botID); ok {
			continue // already cached
		}
		member, err := m.ownerClient.Rest.GetMember(guildID, botID, rest.WithCtx(ctx))
		if err != nil {
			continue // bot not in this guild; skip silently
		}
		m.ownerClient.Caches.AddMember(*member)
	}
}

// ChannelAccessPermissions are what every bot needs in its bound voice channel;
// a raid will not start while any bot is missing one.
const ChannelAccessPermissions = discord.PermissionViewChannel |
	discord.PermissionConnect |
	discord.PermissionSpeak

// botChannelWarning checks whether botUserID has ChannelAccessPermissions in channelID using
// the owner bot's cache (it has guild, channel and role data via IntentGuilds).
// Both channel and member caches are pre-warmed at startup by warmGuildCache.
func (m *Service) botChannelWarning(botUserID, guildID, channelID snowflake.ID) (ChannelAccessWarning, bool) {
	channel, ok := m.ownerClient.Caches.Channel(channelID)
	if !ok {
		return ChannelAccessWarning{}, false
	}
	member, ok := m.ownerClient.Caches.Member(guildID, botUserID)
	if !ok {
		return ChannelAccessWarning{}, false
	}
	perms := m.ownerClient.Caches.MemberPermissionsInChannel(channel, member)
	if !perms.Has(ChannelAccessPermissions) {
		return ChannelAccessWarning{
			BotID:     botUserID,
			ChannelID: channelID,
		}, true
	}
	return ChannelAccessWarning{}, false
}

func (m *Service) newSpeaker(guildID, botUserID snowflake.ID) (*guild.Speaker, error) {
	client, ok := m.poolSvc.GetClientByID(botUserID)
	if !ok {
		return nil, fmt.Errorf("new speaker: no client for bot %s", botUserID)
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return nil, fmt.Errorf("new speaker: no self user for bot %s", botUserID)
	}
	user := selfUser.User
	return &guild.Speaker{
		ID:       user.ID,
		Username: user.Username,
		Enabled:  m.store.IsSpeakerEnabled(guildID, user.ID),
	}, nil
}
