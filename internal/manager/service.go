package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/relay"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// audioChanBuf is the buffer size for Opus frame channels between the voice
// receiver/provider and the relay fan-out goroutines.
const audioChanBuf = 15

// Service orchestrates speaker bots and voice raid sessions.
// It is the sole owner of all GuildStatus state; callers receive safe value copies.
type Service struct {
	mu       sync.RWMutex
	statuses map[snowflake.ID]*domain.GuildStatus // protected by mu

	store       store.Store
	speaker     speaker.SpeakerService
	poolSvc     pool.PoolService
	ownerClient *bot.Client
	test        config.TestConfig
	sessions    *relay.Manager
}

// NewService creates a new manager Service.
func NewService(st store.Store, spk speaker.SpeakerService, poolSvc pool.PoolService, client *bot.Client, test config.TestConfig) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*domain.GuildStatus),
		store:       st,
		speaker:     spk,
		poolSvc:     poolSvc,
		ownerClient: client,
		test:        test,
		sessions:    relay.NewManager(),
	}
}

// ── internal helpers ─────────────────────────────────────────────────────────

// snapshotLocked returns a deep copy of st enriched with live channel/role data.
// Must be called with mu read-locked (store calls are safe; store has its own lock).
func (m *Service) snapshotLocked(guildID snowflake.ID) domain.GuildStatus {
	st, ok := m.statuses[guildID]
	if !ok {
		var ownerID snowflake.ID
		if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
			ownerID = ownerUser.ID
		}
		return domain.GuildStatus{
			GuildID:       guildID,
			OwnerUserID:   ownerID,
			Speakers:      make(map[snowflake.ID]*domain.Speaker),
			BoundChannels: make(map[snowflake.ID]snowflake.ID),
		}
	}

	// Deep-copy the struct so callers cannot race with future mutations.
	snap := *st
	snap.Speakers = make(map[snowflake.ID]*domain.Speaker, len(st.Speakers))
	for k, v := range st.Speakers {
		cp := *v
		snap.Speakers[k] = &cp
	}
	snap.BoundChannels = make(map[snowflake.ID]snowflake.ID, len(st.BoundChannels))
	for k, v := range st.BoundChannels {
		snap.BoundChannels[k] = v
	}
	// Deep-copy the session so the snapshot holds a fully independent copy.
	if st.Session != nil {
		sessionCopy := *st.Session
		sessionCopy.Speakers = make([]*domain.Speaker, len(st.Session.Speakers))
		for i, sp := range st.Session.Speakers {
			cp := *sp
			sessionCopy.Speakers[i] = &cp
		}
		snap.Session = &sessionCopy
	}
	return snap
}

// ── Owner voice channel ───────────────────────────────────────────────────────

// JoinChannel makes the owner bot join the voice channel bound to userID in guildID.
// Returns nil without joining if no channel is bound.
func (m *Service) JoinChannel(ctx context.Context, guildID, userID snowflake.ID) error {
	channelID, ok := m.store.GetBoundChannel(guildID, userID)
	if !ok {
		return nil
	}
	conn := m.ownerClient.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return err
	}
	slog.Info("joined voice channel",
		slog.String("channelID", channelID.String()),
		slog.String("guildID", guildID.String()),
	)
	return nil
}

// LeaveChannel makes the owner bot leave its current voice channel in a guild.
func (m *Service) LeaveChannel(ctx context.Context, guildID, ownerUserID snowflake.ID) {
	if conn := m.ownerClient.VoiceManager.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}
	slog.Info("left voice channel", slog.String("guildID", guildID.String()), slog.String("userID", ownerUserID.String()))
}

// ── Seeding ───────────────────────────────────────────────────────────────────

// SeedExistingSpeakers checks every pool token against each supplied guild and
// registers any speaker bot that is already a member of that guild.
// Call this once on startup so that bots invited in a previous session are
// automatically re-registered.
func (m *Service) SeedExistingSpeakers(guildIDs []snowflake.ID) {
	var ownerID snowflake.ID
	if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
		ownerID = ownerUser.ID
	}

	for _, guildID := range guildIDs {
		m.seedGuildSpeakers(guildID, ownerID)
		// Ensure every known guild has a persistent relay code.
		m.store.GetOrCreateRelayCode(guildID)
	}
}

// SeedGuild seeds speakers and ensures a persistent relay code for a guild.
// Call this when the owner bot first joins a new guild.
func (m *Service) SeedGuild(guildID snowflake.ID) {
	var ownerID snowflake.ID
	if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
		ownerID = ownerUser.ID
	}
	m.seedGuildSpeakers(guildID, ownerID)
	code := m.store.GetOrCreateRelayCode(guildID)
	slog.Info("guild relay code ready",
		slog.String("guildID", guildID.String()),
		slog.String("code", code),
	)
}

func (m *Service) seedGuildSpeakers(guildID, ownerID snowflake.ID) {
	type entry struct {
		sp  *domain.Speaker
		err error
	}
	var entries []entry
	for _, botUserID := range m.poolSvc.GetIDs() {
		if !m.isGuildMember(guildID, botUserID) {
			continue
		}
		sp, err := m.newSpeaker(botUserID)
		entries = append(entries, entry{sp, err})
	}

	m.mu.Lock()
	st, ok := m.statuses[guildID]
	if !ok {
		st = domain.NewGuildStatus(guildID, ownerID)
		m.statuses[guildID] = st
	}
	for _, e := range entries {
		if e.err != nil {
			slog.Warn("seed: failed to register existing speaker bot",
				slog.String("guildID", guildID.String()),
				slog.Any("err", e.err),
			)
			continue
		}
		if _, exists := st.Speakers[e.sp.ID]; !exists {
			st.Speakers[e.sp.ID] = &domain.Speaker{ID: e.sp.ID, Username: e.sp.Username, Enabled: true}
			slog.Info("seed: registered existing speaker bot",
				slog.String("username", e.sp.Username),
				slog.String("guildID", guildID.String()),
			)
		}
	}
	m.mu.Unlock()
}

// ── Speaker management ────────────────────────────────────────────────────────

// HasAvailableToken reports whether the pool has at least one speaker bot
// that has not yet been added to the given guild.
func (m *Service) HasAvailableToken(guildID snowflake.ID) bool {
	_, ok := m.NextSpeakerID(guildID)
	return ok
}

// NextSpeakerID returns the Discord ApplicationID of the next pool speaker
// whose bot has NOT yet joined the guild.
func (m *Service) NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool) {
	m.mu.RLock()
	var registeredIDs map[snowflake.ID]struct{}
	if st := m.statuses[guildID]; st != nil {
		registeredIDs = make(map[snowflake.ID]struct{}, len(st.Speakers))
		for id := range st.Speakers {
			registeredIDs[id] = struct{}{}
		}
	}
	m.mu.RUnlock()

	for _, botUserID := range m.poolSvc.GetIDs() {
		if _, exists := registeredIDs[botUserID]; exists {
			continue // already registered
		}
		if m.isGuildMember(guildID, botUserID) {
			continue // already a guild member on Discord's side
		}
		return botUserID, true
	}
	return 0, false
}

// ToggleSpeaker enables or disables a speaker within a specific guild.
func (m *Service) ToggleSpeaker(guildID, speakerID snowflake.ID, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return fmt.Errorf("guild %s has no registered status", guildID)
	}
	sp, exists := st.Speakers[speakerID]
	if !exists {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
	}
	sp.Enabled = enabled
	return nil
}

// RemoveSpeaker removes a speaker from a guild's status when they leave the server.
func (m *Service) RemoveSpeaker(guildID, userID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return
	}
	delete(st.Speakers, userID)
	slog.Info("speaker removed from guild",
		slog.String("userID", userID.String()),
		slog.String("guildID", guildID.String()),
	)
}

// TrySeedMember checks whether a newly-joined guild member is an unregistered
// pool speaker bot and registers it if so.
func (m *Service) TrySeedMember(guildID, newUserID snowflake.ID) {
	// Resolve speaker info outside the lock (network I/O).
	sp, err := m.newSpeaker(newUserID)
	if err != nil {
		return // not a pool bot or user unresolvable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return
	}
	existing, exists := st.Speakers[newUserID]
	if !exists {
		st.Speakers[newUserID] = &domain.Speaker{ID: sp.ID, Username: sp.Username, Enabled: true}
		slog.Info("member-join: registered speaker bot",
			slog.String("username", sp.Username),
			slog.String("guildID", guildID.String()),
		)
		return
	}
	// Refresh username, preserve per-guild Enabled state.
	existing.Username = sp.Username
	slog.Info("member-join: refreshed speaker username",
		slog.String("username", sp.Username),
		slog.String("guildID", guildID.String()),
	)
}

// ── Channel / role bindings ──────────────────────────────────────────────────

// BindChannel binds a voice channel to a user (speaker or owner) in a guild.
func (m *Service) BindChannel(guildID, userID, channelID snowflake.ID) {
	m.store.BindChannel(guildID, userID, channelID)
}

// UnbindChannel removes the channel binding for a user in a guild.
func (m *Service) UnbindChannel(guildID, userID snowflake.ID) {
	m.store.UnbindChannel(guildID, userID)
}

// GetBoundChannel returns the bound voice channel for a user in a guild.
func (m *Service) GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetBoundChannel(guildID, userID)
}

// BindOwnerChannel binds a voice channel to the owner bot for a guild.
func (m *Service) BindOwnerChannel(guildID, channelID snowflake.ID) {
	ownerUser, ok := m.ownerClient.Caches.SelfUser()
	if !ok {
		slog.Warn("bind owner channel: self-user not yet cached", slog.String("guildID", guildID.String()))
		return
	}
	m.store.BindChannel(guildID, ownerUser.ID, channelID)
}

// UnbindOwnerChannel removes the owner bot's channel binding for a guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	ownerUser, ok := m.ownerClient.Caches.SelfUser()
	if !ok {
		slog.Warn("unbind owner channel: self-user not yet cached", slog.String("guildID", guildID.String()))
		return
	}
	m.store.UnbindChannel(guildID, ownerUser.ID)
}

// GetOwnerChannel returns the bound voice channel for the owner bot in a guild.
func (m *Service) GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool) {
	ownerUser, ok := m.ownerClient.Caches.SelfUser()
	if !ok {
		return 0, false
	}
	return m.store.GetBoundChannel(guildID, ownerUser.ID)
}

// BindCallerRole sets the Discord role whose members' voice will be captured in the guild.
func (m *Service) BindCallerRole(guildID, roleID snowflake.ID) {
	m.store.BindRole(guildID, store.RoleTypeCaller, roleID)
	slog.Info("caller role bound",
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
}

// BindManagerRole sets the Discord role whose members are allowed to setup, start and stop the bot.
func (m *Service) BindManagerRole(guildID, roleID snowflake.ID) {
	m.store.BindRole(guildID, store.RoleTypeManager, roleID)
	slog.Info("manager role bound",
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
}

// HasManagerRole reports whether any of the supplied role IDs matches the
// configured manager role for the guild.
func (m *Service) HasManagerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool {
	managerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeManager)
	if !ok {
		return false
	}
	for _, id := range memberRoleIDs {
		if id == managerRoleID {
			return true
		}
	}
	return false
}

// HasCallerRole reports whether any of the supplied role IDs matches the
// configured caller role for the guild. If no caller role is set, returns true
// so that all users are allowed when the role is unconfigured.
func (m *Service) HasCallerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool {
	callerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	if !ok {
		return true // no restriction configured
	}
	for _, id := range memberRoleIDs {
		if id == callerRoleID {
			return true
		}
	}
	return false
}

// ── Status snapshot ───────────────────────────────────────────────────────────

// GetStatus returns a safe, enriched value snapshot of the guild status.
// The returned value is fully owned by the caller; no locking required after return.
func (m *Service) GetStatus(guildID snowflake.ID) domain.GuildStatus {
	m.mu.RLock()
	snap := m.snapshotLocked(guildID)
	m.mu.RUnlock()

	// Enrich with live channel/role data (store has its own lock; no manager lock needed).
	for spID := range snap.Speakers {
		if chID, ok := m.store.GetBoundChannel(guildID, spID); ok {
			snap.BoundChannels[spID] = chID
		}
	}
	if roleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller); ok {
		snap.CallerRoleID = &roleID
	}
	if managerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeManager); ok {
		snap.ManagerRoleID = &managerRoleID
	}
	if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
		snap.OwnerUserID = ownerUser.ID
		if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
			snap.BoundChannels[ownerUser.ID] = chID
		}
	}
	if code, ok := m.store.GetRelayCode(guildID); ok {
		snap.RelayCode = code
	}
	if guild, ok := m.ownerClient.Caches.Guild(guildID); ok {
		snap.GuildName = guild.Name
	}
	if snap.Session != nil {
		if relaySess, ok := m.sessions.GetByGuild(guildID); ok {
			if snap.Session.IsGuest {
				if hostGuild, ok := m.ownerClient.Caches.Guild(relaySess.HostGuildID); ok {
					snap.HostGuildName = hostGuild.Name
				}
			} else {
				for _, guestID := range relaySess.GuestGuildIDs() {
					name := guestID.String()
					if g, ok := m.ownerClient.Caches.Guild(guestID); ok {
						name = g.Name
					}
					snap.GuestGuildNames = append(snap.GuestGuildNames, name)
				}
			}
		}
	}

	return snap
}

// HasActiveSession reports whether there is a running voice raid for the guild.
func (m *Service) HasActiveSession(guildID snowflake.ID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	return st != nil && st.HasActiveSession()
}

// ── Voice raid helpers ────────────────────────────────────────────────────────

// snapshotSpeakers returns a deep copy of the guild's speaker map.
// Returns an error if the guild has no status or already has an active session.
func (m *Service) snapshotSpeakers(guildID snowflake.ID) (map[snowflake.ID]*domain.Speaker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	if st == nil {
		return nil, fmt.Errorf("no guild status found — seed the guild first")
	}
	if st.Session != nil {
		return nil, fmt.Errorf("a voice raid is already active in this server")
	}
	speakers := make(map[snowflake.ID]*domain.Speaker, len(st.Speakers))
	for k, v := range st.Speakers {
		speakers[k] = new(*v)
	}
	return speakers, nil
}

// joinSpeakers joins all enabled, bound speakers in parallel.
// When withCapture is true each speaker also captures incoming frames.
// All slices (joined, outs, captures, channelIDs) are index-aligned.
func (m *Service) joinSpeakers(ctx context.Context, guildID snowflake.ID, speakers map[snowflake.ID]*domain.Speaker, withCapture bool) ([]*domain.Speaker, []chan []byte, []chan []byte, []snowflake.ID) {
	type candidate struct {
		sp        *domain.Speaker
		channelID snowflake.ID
	}
	var candidates []candidate
	for spID, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		if channelID, ok := m.store.GetBoundChannel(guildID, spID); ok {
			candidates = append(candidates, candidate{sp, channelID})
		}
	}

	type result struct {
		sp        *domain.Speaker
		chOut     chan []byte
		chCapture chan []byte // nil when withCapture is false
		channelID snowflake.ID
	}
	resultCh := make(chan result, len(candidates))
	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for _, c := range candidates {
		go func(sp *domain.Speaker, channelID snowflake.ID) {
			defer wg.Done()
			if err := m.speaker.JoinChannel(ctx, sp.ID, guildID, channelID); err != nil {
				slog.Warn("speaker failed to join channel", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				return
			}
			chOut := make(chan []byte, audioChanBuf)
			var chCapture chan []byte
			if withCapture {
				chCapture = make(chan []byte, audioChanBuf)
			}
			if err := m.speaker.Consume(ctx, sp.ID, guildID, chOut, chCapture); err != nil {
				slog.Error("failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				m.speaker.LeaveChannel(ctx, guildID, sp.ID)
				return
			}
			resultCh <- result{sp, chOut, chCapture, channelID}
		}(c.sp, c.channelID)
	}
	wg.Wait()
	close(resultCh)

	var joined []*domain.Speaker
	var outs []chan []byte
	var captures []chan []byte
	var channelIDs []snowflake.ID
	for r := range resultCh {
		joined = append(joined, r.sp)
		outs = append(outs, r.chOut)
		captures = append(captures, r.chCapture)
		channelIDs = append(channelIDs, r.channelID)
	}
	return joined, outs, captures, channelIDs
}

// commitSession stores session under write lock, re-checking for conflicts.
func (m *Service) commitSession(guildID snowflake.ID, session *domain.VoiceSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statuses[guildID]
	if st == nil {
		return fmt.Errorf("guild status disappeared before session could be stored")
	}
	if st.HasActiveSession() {
		return fmt.Errorf("a voice raid is already active in this server")
	}
	st.Session = session
	return nil
}

// setupOwnerCapture configures the owner connection for audio capture (host mode).
// The owner listens for caller-role-filtered voice frames and writes them into chIn.
// Returns chIn, receiver and provider so the caller can close them on teardown.
func (m *Service) setupOwnerCapture(ctx context.Context, conn voice.Conn, ownerUserID, guildID snowflake.ID) (chan []byte, *opus.VoiceReceiver, *opus.EmptyVoiceProvider, error) {
	caches := m.ownerClient.Caches

	var allowUser func(snowflake.ID) bool
	if roleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller); ok {
		slog.Info("role filter active", slog.String("guildID", guildID.String()), slog.String("roleID", roleID.String()))

		// Pre-fetch full member data for every user currently in the owner's voice
		// channel via a single RequestMembers gateway op. Discord responds with
		// GUILD_MEMBERS_CHUNK events that populate the cache with complete RoleIDs,
		// replacing any partial entries written by earlier VOICE_STATE_UPDATE events.
		if chID := conn.ChannelID(); chID != nil {
			var userIDs []snowflake.ID
			for vs := range caches.VoiceStates(guildID) {
				if vs.ChannelID != nil && *vs.ChannelID == *chID && vs.UserID != ownerUserID {
					userIDs = append(userIDs, vs.UserID)
				}
			}
			if len(userIDs) > 0 {
				if err := m.ownerClient.RequestMembers(ctx, guildID, false, "", userIDs...); err != nil {
					slog.Warn("setupOwnerCapture: RequestMembers failed", slog.Any("err", err))
				}
			}
		}

		allowUser = func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			if !ok {
				return false
			}
			if m.test.IsTestBot(userID) {
				return true
			}
			if member.User.Bot {
				return false
			}
			for _, rID := range member.RoleIDs {
				if rID == roleID {
					return true
				}
			}
			return false
		}
	} else {
		// No role filter — allow all non-bot users.
		allowUser = func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			return ok && !member.User.Bot
		}
	}

	chIn := make(chan []byte, audioChanBuf)
	receiver := opus.NewVoiceReceiver(chIn, ownerUserID, allowUser)
	provider := opus.NewEmptyVoiceProvider()
	conn.SetOpusFrameReceiver(receiver)
	conn.SetOpusFrameProvider(provider)
	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return nil, nil, nil, fmt.Errorf("set speaking flag: %w", err)
	}
	return chIn, receiver, provider, nil
}

// setupOwnerRelay configures the owner connection for audio relay (guest mode).
// The owner plays frames from chOut and discards any incoming voice.
// Returns chOut and provider so the caller can close them on teardown.
func (m *Service) setupOwnerRelay(ctx context.Context, conn voice.Conn) (chan []byte, *opus.VoiceProvider, error) {
	chOut := make(chan []byte, audioChanBuf)
	provider := opus.NewVoiceProvider(chOut)
	conn.SetOpusFrameProvider(provider)
	conn.SetOpusFrameReceiver(opus.NewEmptyVoiceReceiver())
	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return nil, nil, fmt.Errorf("set owner speaking flag: %w", err)
	}
	return chOut, provider, nil
}

// JoinSession connects this guild as a guest to an existing relay session.
// mode must be a guest mode: RaidModeGuestOne (listener only) or RaidModeAllyCaller
// (speakers also capture from their channels for local mixing).
// The session ends automatically when the host ends or ctx is cancelled.
func (m *Service) JoinSession(ctx context.Context, cancelFunc context.CancelFunc, code relay.RelayCode, guestGuildID snowflake.ID, mode domain.RaidMode) error {
	relaySession, err := m.sessions.Join(code, guestGuildID)
	if err != nil {
		return err
	}

	speakers, err := m.snapshotSpeakers(guestGuildID)
	if err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		return err
	}

	joinedSpeakers, outs, _, _ := m.joinSpeakers(ctx, guestGuildID, speakers, mode.WithCapture())

	// Join the owner bot as a relayer into its bound channel.
	var ownerProvider *opus.VoiceProvider
	var ownerUserID snowflake.ID
	if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
		ownerUserID = ownerUser.ID
		if err := m.JoinChannel(ctx, guestGuildID, ownerUser.ID); err != nil {
			slog.Warn("guest: failed to join owner channel", slog.Any("err", err))
		} else if conn := m.ownerClient.VoiceManager.GetConn(guestGuildID); conn != nil {
			chOut, provider, err := m.setupOwnerRelay(ctx, conn)
			if err != nil {
				slog.Warn("guest: failed to setup owner relay", slog.Any("err", err))
			} else {
				ownerProvider = provider
				outs = append(outs, chOut)
			}
		}
	}

	relaySession.AddGuild(guestGuildID, outs)

	session := &domain.VoiceSession{
		GuildID:   guestGuildID,
		Cancel:    cancelFunc,
		RelayCode: code,
		IsGuest:   true,
		Speakers:  joinedSpeakers,
	}
	if err := m.commitSession(guestGuildID, session); err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		return err
	}

	slog.Info("guest joined relay session",
		slog.String("guildID", guestGuildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", code),
		slog.Int("activeSpeakers", len(joinedSpeakers)),
		slog.Bool("ownerRelaying", ownerProvider != nil),
	)

	go func() {
		defer func() {
			// Leave channels before closing audio channels so consumers are not
			// reading from a closed channel while still connected.
			for _, sp := range joinedSpeakers {
				m.speaker.LeaveChannel(context.Background(), guestGuildID, sp.ID)
			}
			if ownerProvider != nil {
				ownerProvider.Close()
				m.LeaveChannel(context.Background(), guestGuildID, ownerUserID)
			}
			for _, out := range outs {
				close(out)
			}
			relaySession.RemoveGuild(guestGuildID)
			m.sessions.RemoveGuest(guestGuildID)
			m.mu.Lock()
			if st := m.statuses[guestGuildID]; st != nil {
				st.Session = nil
			}
			m.mu.Unlock()
			slog.Info("guest session ended", slog.String("guildID", guestGuildID.String()))
		}()
		select {
		case <-ctx.Done():
		case <-relaySession.Done():
			cancelFunc()
		}
	}()

	return nil
}

// Shutdown stops every active voice raid and closes all speaker gateways.
func (m *Service) Shutdown(ctx context.Context) {
	slog.Info("shutting down manager service...")

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
			slog.Warn("shutdown: failed to stop voice raid",
				slog.String("guildID", guildID.String()),
				slog.Any("err", err),
			)
		}
	}
	m.poolSvc.Shutdown(ctx)
}

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
	_, err := m.ownerClient.Rest.GetMember(guildID, userID)
	return err == nil
}

func (m *Service) newSpeaker(botUserID snowflake.ID) (*domain.Speaker, error) {
	user, ok := m.speaker.GetUserByID(botUserID)
	if !ok {
		return nil, fmt.Errorf("cannot resolve user for token")
	}
	return &domain.Speaker{
		ID:       user.ID,
		Username: user.Username,
		Enabled:  true,
	}, nil
}
