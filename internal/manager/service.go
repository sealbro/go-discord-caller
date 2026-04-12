package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/relay"
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

	store      store.Store
	poolSvc    pool.PoolService
	ownerBotID snowflake.ID
	test       config.TestConfig
	sessions   *relay.Manager
}

// NewService creates a new manager Service.
func NewService(st store.Store, poolSvc pool.PoolService, ownerID snowflake.ID, test config.TestConfig) *Service {
	return &Service{
		statuses:   make(map[snowflake.ID]*domain.GuildStatus),
		store:      st,
		poolSvc:    poolSvc,
		ownerBotID: ownerID,
		test:       test,
		sessions:   relay.NewManager(),
	}
}

// getOwnerClient returns the owner bot client from the pool.
// Returns nil if the owner gateway is not connected.
func (m *Service) getOwnerClient() *bot.Client {
	c, _ := m.poolSvc.GetClientByID(m.ownerBotID)
	return c
}

// JoinChannel makes the owner bot join the voice channel bound to userID in guildID.
// Returns nil without joining if no channel is bound.
func (m *Service) JoinChannel(ctx context.Context, guildID, userID snowflake.ID) error {
	channelID, ok := m.store.GetBoundChannel(guildID, userID)
	if !ok {
		return nil
	}
	return m.poolSvc.JoinChannel(ctx, guildID, m.ownerBotID, channelID)
}

// LeaveChannel makes the owner bot leave its current voice channel in a guild.
func (m *Service) LeaveChannel(ctx context.Context, guildID, ownerUserID snowflake.ID) {
	m.poolSvc.LeaveChannel(ctx, guildID, ownerUserID)
}

// SeedExistingSpeakers registers any pool bots already in each guild and ensures
// every guild has a persistent relay code. Call on startup and when the owner
// bot joins a new guild.
func (m *Service) SeedExistingSpeakers(guildIDs []snowflake.ID) {
	for _, guildID := range guildIDs {
		m.seedGuildSpeakers(guildID, m.ownerBotID)
		code := m.store.GetOrCreateRelayCode(guildID)
		slog.Info("guild relay code ready",
			slog.String("guildID", guildID.String()),
			slog.String("code", code),
		)
	}
}

func (m *Service) seedGuildSpeakers(guildID, ownerID snowflake.ID) {
	type initSpeaker struct {
		speaker *domain.Speaker
		err     error
	}
	var speakers []initSpeaker
	for _, botUserID := range m.poolSvc.GetIDs() {
		if !m.isGuildMember(guildID, botUserID) {
			continue
		}
		newSpeaker, err := m.newSpeaker(botUserID)
		speakers = append(speakers, initSpeaker{newSpeaker, err})
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.statuses[guildID]
	if !ok {
		st = domain.NewGuildStatus(guildID, ownerID)
		m.statuses[guildID] = st
	}
	for _, init := range speakers {
		if init.err != nil {
			slog.Warn("seed: failed to register existing speaker bot",
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
	newSpeaker, err := m.newSpeaker(newUserID)
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
		st.Speakers[newUserID] = newSpeaker
		slog.Info("member-join: registered speaker bot",
			slog.String("username", newSpeaker.Username),
			slog.String("guildID", guildID.String()),
		)
		return
	}
	// Refresh username, preserve per-guild Enabled state.
	existing.Username = newSpeaker.Username
	slog.Info("member-join: refreshed speaker username",
		slog.String("username", newSpeaker.Username),
		slog.String("guildID", guildID.String()),
	)
}

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
	m.store.BindChannel(guildID, m.ownerBotID, channelID)
}

// UnbindOwnerChannel removes the owner bot's channel binding for a guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	m.store.UnbindChannel(guildID, m.ownerBotID)
}

// GetOwnerChannel returns the bound voice channel for the owner bot in a guild.
func (m *Service) GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetBoundChannel(guildID, m.ownerBotID)
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
	snap.OwnerUserID = m.ownerBotID
	if chID, ok := m.store.GetBoundChannel(guildID, m.ownerBotID); ok {
		snap.BoundChannels[m.ownerBotID] = chID
	}
	if code, ok := m.store.GetRelayCode(guildID); ok {
		snap.RelayCode = code
	}
	if guild, ok := m.getOwnerClient().Caches.Guild(guildID); ok {
		snap.GuildName = guild.Name
	}
	if snap.Session != nil {
		if relaySess, ok := m.sessions.GetByGuild(guildID); ok {
			if snap.Session.IsGuest {
				if hostGuild, ok := m.getOwnerClient().Caches.Guild(relaySess.HostGuildID); ok {
					snap.HostGuildName = hostGuild.Name
				}
			} else {
				for _, guestID := range relaySess.GuestGuildIDs() {
					name := guestID.String()
					if g, ok := m.getOwnerClient().Caches.Guild(guestID); ok {
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
	_, err := m.getOwnerClient().Rest.GetMember(guildID, userID)
	return err == nil
}

// snapshotLocked returns a deep copy of st enriched with live channel/role data.
// Must be called with mu read-locked (store calls are safe; store has its own lock).
func (m *Service) snapshotLocked(guildID snowflake.ID) domain.GuildStatus {
	st, ok := m.statuses[guildID]
	if !ok {
		return domain.GuildStatus{
			GuildID:       guildID,
			OwnerUserID:   m.ownerBotID,
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

func (m *Service) newSpeaker(botUserID snowflake.ID) (*domain.Speaker, error) {
	client, ok := m.poolSvc.GetClientByID(botUserID)
	if !ok {
		return nil, fmt.Errorf("cannot find client for bot user ID %s", botUserID)
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return nil, fmt.Errorf("cannot find self user for bot user ID %s", botUserID)
	}
	user := selfUser.User
	return &domain.Speaker{
		ID:       user.ID,
		Username: user.Username,
		Enabled:  true,
	}, nil
}
