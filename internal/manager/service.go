package manager

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// audioChanBuf is the buffer size for Opus frame channels between the voice
// receiver/provider and the relay fan-out goroutines.
// 10 frames × 20 ms = 200 ms max buffer depth; drain thresholds handle jitter
// without accumulating large silent latency.
const audioChanBuf = 10

// Service orchestrates speaker bots and voice raid sessions.
// It is the sole owner of all GuildStatus state; callers receive safe value copies.
type Service struct {
	mu       sync.RWMutex
	statuses map[snowflake.ID]*guild.Status // protected by mu

	store       store.Store
	poolSvc     pool.PoolService
	ownerClient *bot.Client
	ownerBotID  snowflake.ID
	test        config.TestConfig
	sessions    *ally.Manager
}

// NewService creates a new manager Service.
func NewService(st store.Store, poolSvc pool.PoolService, ownerClient *bot.Client, ownerID snowflake.ID, test config.TestConfig) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       st,
		poolSvc:     poolSvc,
		ownerClient: ownerClient,
		ownerBotID:  ownerID,
		test:        test,
		sessions:    ally.NewManager(),
	}
}

// StartMetrics registers OTel observable metric callbacks.
// Call once after the speaker pool is connected, alongside StartWatchdog.
func (m *Service) StartMetrics() {
	meter := otel.Meter("go-discord-caller")
	if _, err := meter.RegisterCallback(m.observeBotOnline, telemetry.BotOnline); err != nil {
		slog.Error("manager: failed to register bot_online metric callback", slog.Any("err", err))
	}
}

// observeBotOnline is an OTel observable callback that emits gdc_bot_online
// for every bot (owner + pool speakers) × every known guild at metric collection time.
// Value is 1 when the bot is a registered member of that guild, 0 when not.
func (m *Service) observeBotOnline(_ context.Context, o metric.Observer) error {
	speakerIDs := m.poolSvc.GetIDs()

	m.mu.RLock()
	defer m.mu.RUnlock()

	for guildID, st := range m.statuses {
		// Owner bot is always online in every guild it manages.
		o.ObserveInt64(telemetry.BotOnline, 1,
			metric.WithAttributes(
				attribute.String("user_id", m.ownerBotID.String()),
				attribute.String("guild_id", guildID.String()),
			),
		)

		// Speaker bots: 1 if registered in this guild, 0 if not.
		for _, botID := range speakerIDs {
			value := int64(0)
			if _, inGuild := st.Speakers[botID]; inGuild {
				value = 1
			}
			o.ObserveInt64(telemetry.BotOnline, value,
				metric.WithAttributes(
					attribute.String("user_id", botID.String()),
					attribute.String("guild_id", guildID.String()),
				),
			)
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

// channelHasListeners returns true if channelID contains at least one non-bot
// user according to the owner bot's voice-state cache.
func (m *Service) channelHasListeners(guildID, channelID snowflake.ID) bool {
	for vs := range m.ownerClient.Caches.VoiceStates(guildID) {
		if vs.ChannelID == nil || *vs.ChannelID != channelID {
			continue
		}
		member, ok := m.ownerClient.Caches.Member(guildID, vs.UserID)
		if ok && !member.User.Bot {
			return true
		}
	}
	return false
}

// syncMixerPauseState checks every channel mixer in session and pauses those
// whose destination channel has no non-bot listeners.
func (m *Service) syncMixerPauseState(guildID snowflake.ID, session *guild.Session) {
	for chID, mx := range session.ChannelMixers {
		mx.SetPaused(!m.channelHasListeners(guildID, chID))
	}
}

// UpdateMixerPause is called on voice state changes (join/leave/move) to
// pause or resume channel mixers for the affected guild. Safe to call when
// there is no active session — it is a no-op.
func (m *Service) UpdateMixerPause(guildID snowflake.ID) {
	m.mu.RLock()
	st := m.statuses[guildID]
	if st == nil || st.Session == nil || st.Session.ChannelMixers == nil {
		m.mu.RUnlock()
		return
	}
	// Snapshot the mixer map under the read lock so we can iterate without
	// holding the lock (channelHasListeners acquires sub-locks on the disgo
	// cache, and holding mu there risks a lock-order inversion).
	mixers := make(map[snowflake.ID]guild.MixerPauser, len(st.Session.ChannelMixers))
	maps.Copy(mixers, st.Session.ChannelMixers)
	m.mu.RUnlock()

	for chID, mx := range mixers {
		mx.SetPaused(!m.channelHasListeners(guildID, chID))
	}
}

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
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
		cp := *v
		snap.Speakers[k] = &cp
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
		sessionCopy.Speakers = make([]guild.Speaker, len(st.Session.Speakers))
		copy(sessionCopy.Speakers, st.Session.Speakers)
		snap.Session = &sessionCopy
	}
	return snap
}

func (m *Service) newSpeaker(botUserID snowflake.ID) (*guild.Speaker, error) {
	client, ok := m.poolSvc.GetClientByID(botUserID)
	if !ok {
		return nil, fmt.Errorf("cannot find client for bot user ID %s", botUserID)
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return nil, fmt.Errorf("cannot find self user for bot user ID %s", botUserID)
	}
	user := selfUser.User
	return &guild.Speaker{
		ID:       user.ID,
		Username: user.Username,
		Enabled:  true,
	}, nil
}
