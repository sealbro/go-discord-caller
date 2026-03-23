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
	speaker     *speaker.Service
	poolSvc     *pool.Service
	ownerClient *bot.Client
}

// NewService creates a new manager Service.
func NewService(st store.Store, spk *speaker.Service, poolSvc *pool.Service, client *bot.Client) *Service {
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
		ownerUser, _ := m.ownerClient.Caches.SelfUser()
		st = domain.NewGuildStatus(guildID, ownerUser.ID)
		m.statuses[guildID] = st
	}
	return st
}

// snapshotLocked returns a deep copy of st enriched with live channel/role data.
// Must be called with mu read-locked (store calls are safe; store has its own lock).
func (m *Service) snapshotLocked(guildID snowflake.ID) domain.GuildStatus {
	st, ok := m.statuses[guildID]
	if !ok {
		ownerUser, _ := m.ownerClient.Caches.SelfUser()
		return domain.GuildStatus{
			GuildID:       guildID,
			OwnerUserID:   ownerUser.ID,
			Speakers:      make(map[snowflake.ID]*domain.Speaker),
			BoundChannels: make(map[snowflake.ID]snowflake.ID),
		}
	}

	// Deep-copy the struct so callers cannot race with future mutations.
	snap := *st
	snap.Speakers = make(map[snowflake.ID]*domain.Speaker, len(st.Speakers))
	for k, v := range st.Speakers {
		sp := *v
		snap.Speakers[k] = &sp
	}
	snap.BoundChannels = make(map[snowflake.ID]snowflake.ID, len(st.BoundChannels))
	for k, v := range st.BoundChannels {
		snap.BoundChannels[k] = v
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
	ownerUser, _ := m.ownerClient.Caches.SelfUser()

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

		// Now apply under write lock.
		m.mu.Lock()
		st := domain.NewGuildStatus(guildID, ownerUser.ID)
		for _, e := range entries {
			if e.err != nil {
				slog.Warn("seed: failed to register existing speaker bot",
					slog.String("guildID", guildID.String()),
					slog.Any("err", e.err),
				)
				continue
			}
			st.Speakers[e.sp.ID] = &domain.Speaker{ID: e.sp.ID, Username: e.sp.Username, Enabled: true}
			slog.Info("seed: registered existing speaker bot",
				slog.String("username", e.sp.Username),
				slog.String("guildID", guildID.String()),
			)
		}
		m.statuses[guildID] = st
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
	st := m.statuses[guildID]
	m.mu.RUnlock()

	for _, botUserID := range m.poolSvc.GetIDs() {
		if st != nil {
			m.mu.RLock()
			_, exists := st.Speakers[botUserID]
			m.mu.RUnlock()
			if exists {
				continue // already registered
			}
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
		return // not previously registered in this guild
	}
	// Refresh username, preserve per-guild Enabled state.
	existing.Username = sp.Username
	slog.Info("member-join: registered speaker bot",
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
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.BindChannel(guildID, ownerUser.ID, channelID)
}

// UnbindOwnerChannel removes the owner bot's channel binding for a guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.UnbindChannel(guildID, ownerUser.ID)
}

// GetOwnerChannel returns the bound voice channel for the owner bot in a guild.
func (m *Service) GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	return m.store.GetBoundChannel(guildID, ownerUser.ID)
}

// BindRole sets the Discord role whose members' voice will be captured in the guild.
func (m *Service) BindRole(guildID, roleID snowflake.ID) {
	m.store.BindRole(guildID, roleID)
	slog.Info("role bound",
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
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
	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		snap.RoleID = &roleID
	}
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	snap.OwnerUserID = ownerUser.ID
	if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
		snap.BoundChannels[ownerUser.ID] = chID
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

	ownerUser, _ := m.ownerClient.Caches.SelfUser()
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
	receiver := opus.NewVoiceReceiver(chIn, ownerUser.ID)
	conn.SetOpusFrameReceiver(receiver)
	provider := opus.NewEmptyVoiceProvider()
	conn.SetOpusFrameProvider(provider)

	var outs []chan []byte
	newOut := func() chan []byte {
		ch := make(chan []byte, 10)
		outs = append(outs, ch)
		return ch
	}

	session := &domain.VoiceSession{GuildID: guildID, Cancel: cancelFunc}

	for spID, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		channelID, hasChannel := m.store.GetBoundChannel(guildID, spID)
		if !hasChannel {
			continue
		}
		if err := m.speaker.JoinChannel(ctx, spID, guildID, channelID); err != nil {
			slog.Warn("speaker failed to join channel on raid start",
				slog.String("speakerID", spID.String()),
				slog.Any("err", err),
			)
			continue
		}
		session.Speakers = append(session.Speakers, sp)

		chOut := newOut()
		if err := m.speaker.Consume(ctx, spID, guildID, chOut); err != nil {
			slog.Error("failed to consume voice data", slog.String("speakerID", spID.String()), slog.Any("err", err))
		}
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
			provider.Close()
			receiver.Close()
			for _, out := range outs {
				close(out)
			}
			close(chIn)
			slog.Info("voice raid ended", slog.String("guildID", guildID.String()))
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-chIn:
				if !ok {
					return
				}
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
	status.Session = nil
	m.mu.Unlock()

	for _, sp := range session.Speakers {
		m.speaker.LeaveChannel(ctx, guildID, sp.ID)
	}
	m.LeaveChannel(ctx, guildID, status.OwnerUserID)
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
