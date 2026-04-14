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

	store       store.Store
	poolSvc     pool.PoolService
	ownerClient *bot.Client
	ownerBotID  snowflake.ID
	test        config.TestConfig
	sessions    *relay.Manager
}

// NewService creates a new manager Service.
func NewService(st store.Store, poolSvc pool.PoolService, ownerClient *bot.Client, ownerID snowflake.ID, test config.TestConfig) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*domain.GuildStatus),
		store:       st,
		poolSvc:     poolSvc,
		ownerClient: ownerClient,
		ownerBotID:  ownerID,
		test:        test,
		sessions:    relay.NewManager(),
	}
}

// ownerVoice returns a GuildVoice for the owner bot in guildID, bound to its
// configured channel. Use Join/Leave on the result to manage the connection.
func (m *Service) ownerVoice(guildID snowflake.ID) pool.GuildVoice {
	channelID, _ := m.store.GetBoundChannel(guildID, m.ownerBotID)
	return pool.NewGuildVoice(m.ownerClient.VoiceManager, channelID)
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

// leaveSpeaker makes the speaker bot leave its current voice channel in the guild.
func (m *Service) leaveSpeaker(ctx context.Context, guildID, botUserID snowflake.ID) {
	if gv, ok := m.speakerVoice(guildID, botUserID); ok {
		gv.Leave(ctx, guildID)
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
	// Nil out Cancel/Cleanup — snapshots are read-only display objects.
	if st.Session != nil {
		sessionCopy := *st.Session
		sessionCopy.Cancel = nil
		sessionCopy.Cleanup = nil
		sessionCopy.Speakers = make([]domain.Speaker, len(st.Session.Speakers))
		copy(sessionCopy.Speakers, st.Session.Speakers)
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
