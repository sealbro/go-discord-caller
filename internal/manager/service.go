package manager

import (
	"context"
	"fmt"
	"log/slog"

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
type Service struct {
	store       store.Store
	statusStore store.StatusStore
	speakers    store.SpeakerStore
	speaker     *speaker.Service
	poolSvc     *pool.Service
	ownerClient *bot.Client
}

// NewService creates a new manager Service.
func NewService(st store.Store, statusStore store.StatusStore, speakers store.SpeakerStore, spk *speaker.Service, poolSvc *pool.Service, client *bot.Client) *Service {
	return &Service{
		store:       st,
		statusStore: statusStore,
		speakers:    speakers,
		speaker:     spk,
		poolSvc:     poolSvc,
		ownerClient: client,
	}
}

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
func (m *Service) LeaveChannel(ctx context.Context, guildID snowflake.ID) {
	if conn := m.ownerClient.VoiceManager.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}

	slog.Info("left voice channel", slog.String("guildID", guildID.String()))
}

// SeedExistingSpeakers checks every pool token against each supplied guild and
// registers any speaker bot that is already a member of that guild but is not
// yet tracked in the status store. Call this once on startup so that bots
// invited in a previous session are automatically re-registered.
func (m *Service) SeedExistingSpeakers(guildIDs []snowflake.ID) {
	for _, guildID := range guildIDs {
		status := domain.NewGuildStatus(guildID)
		for _, botUserID := range m.poolSvc.GetIDs() {
			if !m.isGuildMember(guildID, botUserID) {
				continue
			}
			sp, err := m.AddSpeaker(guildID, botUserID)
			if err != nil {
				slog.Warn("seed: failed to register existing speaker bot",
					slog.String("guildID", guildID.String()),
					slog.Any("err", err),
				)
				continue
			}

			status.Speakers = append(status.Speakers, sp)
			status.Enabled[botUserID] = sp.Enabled
			slog.Info("seed: registered existing speaker bot",
				slog.String("username", sp.Username),
				slog.String("guildID", guildID.String()),
			)
		}
		m.statusStore.SetStatus(status)
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
	status := m.statusStore.GetStatus(guildID)
	for _, botUserID := range m.poolSvc.GetIDs() {
		if _, exists := status.Enabled[botUserID]; exists {
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
	status := m.statusStore.GetStatus(guildID)
	if _, exists := status.Enabled[speakerID]; !exists {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
	}
	status.Enabled[speakerID] = enabled
	m.statusStore.SetStatus(status)
	return nil
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

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
func (m *Service) StartVoiceRaid(mainCtx context.Context, guildID snowflake.ID) error {
	status := m.statusStore.GetStatus(guildID)
	if status.HasActiveSession() {
		return fmt.Errorf("a voice raid is already active in this server")
	}

	ctx, cancelFunc := context.WithCancel(mainCtx)

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

	for spID, enabled := range status.Enabled {
		if !enabled {
			continue
		}
		channelID, hasChannel := m.store.GetBoundChannel(guildID, spID)
		if !hasChannel {
			continue
		}
		sp, ok := m.speakers.GetSpeaker(spID)
		if !ok {
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

	status.SetSession(session)
	m.statusStore.SetStatus(status)

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
	status := m.statusStore.GetStatus(guildID)
	if !status.HasActiveSession() {
		return fmt.Errorf("no active voice raid in this server")
	}
	for _, sp := range status.Session.Speakers {
		m.speaker.LeaveChannel(ctx, guildID, sp.ID)
	}
	m.LeaveChannel(ctx, guildID)

	status.Session.Cancel()
	status.SetSession(nil)

	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// Shutdown stops every active voice raid and closes all speaker gateways.
func (m *Service) Shutdown(ctx context.Context) {
	slog.Info("shutting down manager service...")

	for _, status := range m.statusStore.ListStatuses() {
		if !status.HasActiveSession() {
			continue
		}
		if err := m.StopVoiceRaid(ctx, status.GuildID); err != nil {
			slog.Warn("shutdown: failed to stop voice raid",
				slog.String("guildID", status.GuildID.String()),
				slog.Any("err", err),
			)
		}
	}
	m.poolSvc.Shutdown(ctx)
}

// GetStatus builds the Status view for a guild from StatusStore, Store, and SessionStore.
func (m *Service) GetStatus(guildID snowflake.ID) *domain.GuildStatus {
	status := m.statusStore.GetStatus(guildID)

	allSpeakers := make([]*domain.Speaker, 0, len(status.Speakers))
	for _, speaker := range status.Speakers {
		if sp, ok := m.speakers.GetSpeaker(speaker.ID); ok {
			allSpeakers = append(allSpeakers, sp)
		}
		if chID, ok := m.store.GetBoundChannel(guildID, speaker.ID); ok {
			status.BoundChannels[speaker.ID] = chID
		}
	}

	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		status.RoleID = &roleID
	}
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
		status.OwnerChannelID = &chID
	}

	return status
}

// TrySeedMember checks whether a newly-joined guild member is an unregistered
// pool speaker bot and registers it if so.
func (m *Service) TrySeedMember(guildID, newUserID snowflake.ID) {
	status := m.statusStore.GetStatus(guildID)
	for _, speaker := range status.Speakers {
		if speaker.ID != newUserID {
			continue
		}
		sp, err := m.AddSpeaker(guildID, speaker.ID)
		if err != nil {
			slog.Warn("member-join: failed to register speaker bot",
				slog.String("guildID", guildID.String()),
				slog.String("userID", speaker.ID.String()),
				slog.Any("err", err),
			)
			return
		}
		slog.Info("member-join: registered speaker bot",
			slog.String("username", sp.Username),
			slog.String("guildID", guildID.String()),
		)
		return
	}
}

// AddSpeaker registers a speaker bot for the given guild and marks it enabled in StatusStore.
func (m *Service) AddSpeaker(guildID, botUserID snowflake.ID) (*domain.Speaker, error) {
	sp, ok := m.speakers.GetSpeaker(botUserID)
	if !ok {
		user, ok := m.speaker.GetUserByID(botUserID)
		if !ok {
			return nil, fmt.Errorf("cannot resolve user for token")
		}
		sp = &domain.Speaker{
			ID:       user.ID,
			Username: user.Username,
			Enabled:  true,
		}
		m.speakers.AddSpeaker(sp)
	}

	slog.Info("speaker added",
		slog.String("username", sp.Username),
		slog.String("guildID", guildID.String()),
	)
	return sp, nil
}

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
	_, err := m.ownerClient.Rest.GetMember(guildID, userID)
	return err == nil
}
