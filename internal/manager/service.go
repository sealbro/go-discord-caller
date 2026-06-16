package manager

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
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
	test               config.TestConfig
	sessions           *ally.Manager
	metrics            *telemetry.Metrics
	reconnect          reconnectState // typed reconnect subsystem (applier registry + in-flight guard)
	sessionIdleTimeout time.Duration  // 0 disables; set via SetSessionIdleTimeout
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

// clearActiveRouter removes a guild from the activeRouters snapshot. Called
// from StopVoiceRaid / JoinSession-teardown under m.mu.
func (m *Service) clearActiveRouter(guildID snowflake.ID) {
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

// SetSessionIdleTimeout configures the duration after which a session whose
// every channel mixer has been continuously paused is auto-stopped.
// Pass 0 to disable. Call once at startup, before any session is started.
func (m *Service) SetSessionIdleTimeout(d time.Duration) {
	m.sessionIdleTimeout = d
}

// startSessionIdleWatcher launches a goroutine that cancels the session when
// every channel mixer has been paused continuously for m.sessionIdleTimeout.
// Safe to call for any session — it no-ops when the timeout is disabled, the
// session has no channel mixers (direct-passthrough mode), or no mixer
// satisfies opus.PauseProbe.
func (m *Service) startSessionIdleWatcher(ctx context.Context, cancelFunc context.CancelFunc, session *guild.Session) {
	if m.sessionIdleTimeout <= 0 || len(session.ChannelMixers) == 0 {
		return
	}
	probes := make([]opus.PauseProbe, 0, len(session.ChannelMixers))
	for _, mx := range session.ChannelMixers {
		if p, ok := mx.(opus.PauseProbe); ok {
			probes = append(probes, p)
		}
	}
	if len(probes) == 0 {
		return
	}
	go opus.NewSessionIdleWatcher(probes, cancelFunc, m.sessionIdleTimeout).Run(ctx)
}

// StartMetrics registers OTel observable metric callbacks.
// Call once after the speaker pool is connected, alongside StartWatchdog.
func (m *Service) StartMetrics() {
	if err := m.metrics.Bot.RegisterBotOnline(m.observeBotOnline); err != nil {
		slog.Error("manager: failed to register bot_online metric callback", slog.Any("err", err))
	}
	if err := m.metrics.Bot.RegisterGuildInfo(m.observeGuildInfo); err != nil {
		slog.Error("manager: failed to register guild_info metric callback", slog.Any("err", err))
	}
}

// observeGuildInfo is an OTel observable callback that emits gdc_discord_guild
// for every guild the owner bot is currently a member of.
// Reads directly from the disgo cache — no duplicate state needed.
func (m *Service) observeGuildInfo(_ context.Context, o metric.Observer) error {
	for g := range m.ownerClient.Caches.Guilds() {
		m.metrics.Bot.ObserveGuildInfo(o, g.ID.String(), g.Name)
	}
	return nil
}

// observeBotOnline is an OTel observable callback that emits gdc_bot_online
// for every bot that is a registered guild member at metric collection time.
// Only emits when value is 1 (bot present in guild) — absent bots produce no
// series rather than a 0, keeping cardinality bounded as pool/guild count grows.
func (m *Service) observeBotOnline(_ context.Context, o metric.Observer) error {
	speakerIDs := m.poolSvc.GetIDs()

	// Snapshot the per-guild speaker sets under the read lock so that metric
	// emission (which calls into the OTel SDK and may itself acquire locks)
	// does not hold m.mu and block voice raid write operations.
	type guildEntry struct {
		guildID    snowflake.ID
		speakerIDs []snowflake.ID
	}
	m.mu.RLock()
	snap := make([]guildEntry, 0, len(m.statuses))
	for guildID, st := range m.statuses {
		var inGuild []snowflake.ID
		for _, botID := range speakerIDs {
			if _, ok := st.Speakers[botID]; ok {
				inGuild = append(inGuild, botID)
			}
		}
		snap = append(snap, guildEntry{guildID, inGuild})
	}
	m.mu.RUnlock()

	for _, e := range snap {
		// Owner bot is always a member of every guild it manages.
		m.metrics.Bot.ObserveBotOnline(o, m.ownerBotID.String(), e.guildID.String())
		for _, botID := range e.speakerIDs {
			m.metrics.Bot.ObserveBotOnline(o, botID.String(), e.guildID.String())
		}
	}
	return nil
}

// ownerVoice returns a GuildVoice for the owner bot in guildID, bound to its
// configured channel. Use Join/Leave on the result to manage the connection.
func (m *Service) ownerVoice(guildID snowflake.ID) pool.GuildVoice {
	channelID, _ := m.store.GetBoundChannel(guildID, m.ownerBotID)
	return pool.NewGuildVoice(m.ownerClient.VoiceManager, channelID)
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
		newSpeaker, err := m.newSpeaker(botUserID)
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

	for _, guildID := range activeGuilds {
		if err := m.StopVoiceRaid(ctx, guildID); err != nil {
			slog.WarnContext(ctx, "shutdown: failed to stop voice raid",
				slog.String("guildID", guildID.String()),
				slog.Any("err", err),
			)
		}
	}
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
	caches := p.svc.ownerClient.Caches
	for vs := range caches.VoiceStates(p.guildID) {
		if vs.ChannelID == nil || *vs.ChannelID != channelID {
			continue
		}
		member, ok := caches.Member(p.guildID, vs.UserID)
		if ok && !p.svc.IsBot(member.User) {
			return true
		}
	}
	return false
}

// EnumerateCallers returns the user IDs of cached non-bot members in
// channelID whose role set contains roleID. When roleID == 0 (no caller role
// bound) every non-bot in the channel counts — matching AllowFilter semantics.
func (p *cacheVoiceProbe) EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID {
	caches := p.svc.ownerClient.Caches
	var users []snowflake.ID
	for vs := range caches.VoiceStates(p.guildID) {
		if vs.ChannelID == nil || *vs.ChannelID != channelID {
			continue
		}
		member, ok := caches.Member(p.guildID, vs.UserID)
		if !ok || p.svc.IsBot(member.User) {
			continue
		}
		if roleID == 0 {
			users = append(users, vs.UserID)
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
	client, ok := m.poolSvc.GetClientByID(botUserID)
	if !ok {
		return pool.GuildVoice{}, false
	}
	channelID, _ := m.store.GetBoundChannel(guildID, botUserID)
	return pool.NewGuildVoice(client.VoiceManager, channelID), true
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
		for _, ch := range channels {
			m.ownerClient.Caches.AddChannel(ch)
		}
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

// botChannelWarning checks whether botUserID has ViewChannel+Connect+Speak in channelID using
// the owner bot's cache (it has full guild data via IntentGuilds + FlagsAll).
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
	if !perms.Has(discord.PermissionViewChannel) || !perms.Has(discord.PermissionConnect) || !perms.Has(discord.PermissionSpeak) {
		return ChannelAccessWarning{
			BotID:     botUserID,
			ChannelID: channelID,
		}, true
	}
	return ChannelAccessWarning{}, false
}

func (m *Service) newSpeaker(botUserID snowflake.ID) (*guild.Speaker, error) {
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
		Enabled:  true,
	}, nil
}
