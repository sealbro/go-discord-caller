package speaker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service manages the lifecycle of speaker bot gateway connections and audio relay.
// Client and Cancel are stored directly on each domain.Speaker.
type Service struct {
	mu      sync.RWMutex
	store   store.Store
	poolSvc *pool.Service
}

// NewService creates a new speaker Service.
func NewService(st store.Store, poolSvc *pool.Service) *Service {
	return &Service{
		store:   st,
		poolSvc: poolSvc,
	}
}

// GetUserByToken returns the discord.User of the bot for the given pool token
// by reading the self-user from the pre-connected gateway's cache.
// Returns a zero User and false if the client is not in the pool or the
// self-user is not yet available.
func (s *Service) GetUserByToken(token string) (discord.User, bool) {
	client, ok := s.poolSvc.GetClientByToken(token)
	if !ok {
		return discord.User{}, false
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return discord.User{}, false
	}
	return selfUser.User, true
}

// AssignClient assigns an already-open pool gateway to the speaker, or opens a new one.
// The client is stored directly on sp.Client.
func (s *Service) AssignClient(sp *domain.Speaker) error {
	if sp.Client != nil {
		return nil // already assigned
	}

	if poolClient, ok := s.poolSvc.GetClientByToken(sp.BotToken); ok {
		sp.Client = poolClient
		slog.Info("speaker assigned from pool",
			slog.String("speakerID", sp.ID.String()),
			slog.String("username", sp.Username),
		)
	}
	return nil
}

// Disconnect cancels any active relay and closes the gateway for the given speaker.
func (s *Service) Disconnect(ctx context.Context, speakerID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sp, ok := s.store.GetSpeaker(speakerID)
	if !ok {
		return
	}

	if sp.Cancel != nil {
		sp.Cancel()
		sp.Cancel = nil
	}

	if sp.Client != nil {
		sp.Client.Close(ctx)
		sp.Client = nil
		slog.Info("speaker disconnected", slog.String("speakerID", speakerID.String()))
	}
}

// JoinChannel makes the speaker bot join the given voice channel.
func (s *Service) JoinChannel(ctx context.Context, speakerID, guildID, channelID snowflake.ID) error {
	s.mu.RLock()
	sp, ok := s.store.GetSpeaker(speakerID)
	s.mu.RUnlock()

	if !ok || sp.Client == nil {
		return fmt.Errorf("speaker %s is not connected", speakerID)
	}

	conn := sp.Client.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return fmt.Errorf("speaker %s join channel %s: %w", speakerID, channelID, err)
	}

	slog.Info("speaker joined channel",
		slog.String("speakerID", speakerID.String()),
		slog.String("channelID", channelID.String()),
	)
	return nil
}

// Consume starts the audio relay for the speaker; the cancel func is stored on sp.Cancel.
func (s *Service) Consume(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte) error {
	s.mu.Lock()
	sp, ok := s.store.GetSpeaker(speakerID)
	if !ok || sp.Client == nil {
		s.mu.Unlock()
		return fmt.Errorf("speaker %s is not connected", speakerID)
	}

	relayCtx, cancel := context.WithCancel(ctx)
	sp.Cancel = cancel
	client := sp.Client
	s.mu.Unlock()

	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("speaker %s is not connected to a voice channel in guild %s", speakerID, guildID)
	}
	provider := opus.NewVoiceProvider(chOut)
	conn.SetOpusFrameProvider(provider)
	receiver := opus.NewEmptyVoiceReceiver()
	conn.SetOpusFrameReceiver(receiver)

	go func() {
		<-relayCtx.Done()
		provider.Close()
		receiver.Close()
	}()

	if err := conn.SetSpeaking(relayCtx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

	return nil
}

// LeaveChannel makes the speaker bot leave its current voice channel.
func (s *Service) LeaveChannel(ctx context.Context, speakerID, guildID snowflake.ID) {
	s.mu.Lock()
	sp, ok := s.store.GetSpeaker(speakerID)
	if !ok {
		s.mu.Unlock()
		return
	}

	if sp.Cancel != nil {
		sp.Cancel()
		sp.Cancel = nil
	}
	client := sp.Client
	s.mu.Unlock()

	if client == nil {
		return
	}

	if conn := client.VoiceManager.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}

	slog.Info("speaker left channel", slog.String("speakerID", speakerID.String()))
}
