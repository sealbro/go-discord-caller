package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service orchestrates speaker bots and voice raid sessions.
// It is the sole owner of all GuildStatus state; callers receive safe value copies.
type Service struct {
	mu       sync.RWMutex
	statuses map[snowflake.ID]*domain.GuildStatus // protected by mu

	store       store.Store
	speaker     speaker.SpeakerService
	poolSvc     pool.PoolService
	ownerClient *bot.Client
}

// NewService creates a new manager Service.
func NewService(st store.Store, spk speaker.SpeakerService, poolSvc pool.PoolService, client *bot.Client) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*domain.GuildStatus),
		store:       st,
		speaker:     spk,
		poolSvc:     poolSvc,
		ownerClient: client,
	}
}

// ── internal helpers ─────────────────────────────────────────────────────────

// getOrCreateLocked returns the live status for guildID, creating it if absent.
// Must be called with mu write-locked.
func (m *Service) getOrCreateLocked(guildID snowflake.ID) *domain.GuildStatus {
	st, ok := m.statuses[guildID]
	if !ok {
		var ownerID snowflake.ID
		if ownerUser, ok := m.ownerClient.Caches.SelfUser(); ok {
			ownerID = ownerUser.ID
		}
		st = domain.NewGuildStatus(guildID, ownerID)
		m.statuses[guildID] = st
	}
	return st
}

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
		snap.Speakers[k] = new(*v)
	}
	snap.BoundChannels = make(map[snowflake.ID]snowflake.ID, len(st.BoundChannels))
	for k, v := range st.BoundChannels {
		snap.BoundChannels[k] = v
	}
	// Deep-copy the session so the snapshot holds a fully independent copy.
	if st.Session != nil {
		sessionCopy := *st.Session
		sessionCopy.Speakers = make([]*domain.Speaker, len(st.Session.Speakers))
		copy(sessionCopy.Speakers, st.Session.Speakers)
		snap.Session = &sessionCopy
	}
	return snap
}

// ── Owner voice channel ───────────────────────────────────────────────────────

// JoinChannel makes the owner bot join a voice channel.
func (m *Service) JoinChannel(ctx context.Context, guildID, channelID snowflake.ID) error {
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
		// Collect speakers first (I/O outside the lock).
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

	return snap
}

// HasActiveSession reports whether there is a running voice raid for the guild.
func (m *Service) HasActiveSession(guildID snowflake.ID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	return st != nil && st.HasActiveSession()
}

// ── Voice raid ────────────────────────────────────────────────────────────────

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
// The caller owns ctx and cancelFunc: cancelFunc is called by the caller on error,
// or stored in the session for StopVoiceRaid to call on success.
func (m *Service) StartVoiceRaid(ctx context.Context, cancelFunc context.CancelFunc, guildID snowflake.ID) error {
	// Take a speaker snapshot under read lock; do all I/O outside the lock.
	m.mu.RLock()
	st := m.statuses[guildID]
	if st == nil {
		m.mu.RUnlock()
		return fmt.Errorf("no guild status found — seed the guild first")
	}
	if st.Session != nil {
		m.mu.RUnlock()
		return fmt.Errorf("a voice raid is already active in this server")
	}
	speakers := make(map[snowflake.ID]*domain.Speaker, len(st.Speakers))
	for k, v := range st.Speakers {
		sp := *v
		speakers[k] = &sp
	}
	m.mu.RUnlock()

	ownerUser, ok := m.ownerClient.Caches.SelfUser()
	if !ok {
		return fmt.Errorf("owner bot self-user not yet cached")
	}
	if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
		if err := m.JoinChannel(ctx, guildID, chID); err != nil {
			return fmt.Errorf("failed to join owner channel: %w", err)
		}
	}

	conn := m.ownerClient.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("no voice connection to join owner channel")
	}

	chIn := make(chan []byte, 10)

	// Build an optional role filter: if a capture role is configured for this guild,
	// close over a live lookup against the member cache so that role changes
	// (grant/revoke) take effect on the next received frame without restarting the raid.
	var allowUser func(snowflake.ID) bool
	if roleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller); ok {
		slog.Info("role filter active",
			slog.String("guildID", guildID.String()),
			slog.String("roleID", roleID.String()),
		)
		caches := m.ownerClient.Caches
		allowUser = func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			if !ok {
				return false
			}
			for _, rID := range member.RoleIDs {
				if rID == roleID {
					return true
				}
			}
			return false
		}
	}

	receiver := opus.NewVoiceReceiver(chIn, ownerUser.ID, allowUser)
	conn.SetOpusFrameReceiver(receiver)
	provider := opus.NewEmptyVoiceProvider()
	conn.SetOpusFrameProvider(provider)

	// Collect eligible speakers before spawning goroutines so map iteration
	// is not mixed with concurrent reads.
	type eligible struct {
		sp        *domain.Speaker
		channelID snowflake.ID
	}
	var candidates []eligible
	for spID, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		channelID, hasChannel := m.store.GetBoundChannel(guildID, spID)
		if !hasChannel {
			continue
		}
		candidates = append(candidates, eligible{sp, channelID})
	}

	type joinResult struct {
		sp    *domain.Speaker
		chOut chan []byte
	}
	resultCh := make(chan joinResult, len(candidates))

	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for _, c := range candidates {
		go func(sp *domain.Speaker, channelID snowflake.ID) {
			defer wg.Done()
			speakerID := sp.ID
			if err := m.speaker.JoinChannel(ctx, speakerID, guildID, channelID); err != nil {
				slog.Warn("speaker failed to join channel on raid start",
					slog.String("speakerID", speakerID.String()),
					slog.Any("err", err),
				)
				return
			}
			chOut := make(chan []byte, 10)
			if err := m.speaker.Consume(ctx, speakerID, guildID, chOut); err != nil {
				slog.Error("failed to consume voice data", slog.String("speakerID", speakerID.String()), slog.Any("err", err))
				m.speaker.LeaveChannel(ctx, guildID, speakerID)
				return
			}
			resultCh <- joinResult{sp, chOut}
		}(c.sp, c.channelID)
	}
	wg.Wait()
	close(resultCh)

	var outs []chan []byte
	session := &domain.VoiceSession{GuildID: guildID, Cancel: cancelFunc}
	for r := range resultCh {
		session.Speakers = append(session.Speakers, r.sp)
		outs = append(outs, r.chOut)
	}

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

	// Write-lock to set the session; re-check to guard against concurrent starts.
	m.mu.Lock()
	st = m.statuses[guildID]
	if st == nil {
		m.mu.Unlock()
		return fmt.Errorf("guild status disappeared before session could be stored")
	}
	if st.HasActiveSession() {
		m.mu.Unlock()
		return fmt.Errorf("a voice raid is already active in this server")
	}
	st.Session = session
	m.mu.Unlock()

	slog.Info("voice raid started",
		slog.String("guildID", guildID.String()),
		slog.Int("activeSpeakers", len(session.Speakers)),
	)

	go func() {
		defer func() {
			// Close receiver first so no new frames are sent to chIn after we stop draining it.
			receiver.Close()
			provider.Close()
			// Close each speaker output channel to signal VoiceProviders to stop.
			for _, out := range outs {
				close(out)
			}
			// chIn is intentionally not closed here: VoiceReceiver.Close() guarantees
			// no further sends, so the channel is safe to let the GC reclaim.
			slog.Info("voice raid ended", slog.String("guildID", guildID.String()))
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt := <-chIn:
				for _, out := range outs {
					select {
					case out <- pkt:
					default:
					}
				}
			}
		}
	}()

	return nil
}

// StopVoiceRaid makes all active speakers leave their voice channels.
func (m *Service) StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	// Extract and clear the session under write lock; do I/O outside.
	m.mu.Lock()
	status := m.statuses[guildID]
	if status == nil || !status.HasActiveSession() {
		m.mu.Unlock()
		return fmt.Errorf("no active voice raid in this server")
	}
	session := status.Session
	ownerUserID := status.OwnerUserID
	status.Session = nil
	m.mu.Unlock()

	for _, sp := range session.Speakers {
		m.speaker.LeaveChannel(ctx, guildID, sp.ID)
	}
	m.LeaveChannel(ctx, guildID, ownerUserID)
	session.Cancel()

	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
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
