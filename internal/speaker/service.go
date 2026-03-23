package speaker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// Service manages the lifecycle of speaker bot gateway connections and audio relay.
// Client and Cancel are stored directly on each domain.Speaker.
type Service struct {
	mu      sync.RWMutex
	poolSvc *pool.Service
}

// NewService creates a new speaker Service.
func NewService(poolSvc *pool.Service) *Service {
	return &Service{
		poolSvc: poolSvc,
	}
}

// GetUserByID returns the discord.User of the bot for the given pool token
// by reading the self-user from the pre-connected gateway's cache.
// Returns a zero User and false if the client is not in the pool or the
// self-user is not yet available.
func (s *Service) GetUserByID(botUserID snowflake.ID) (discord.User, bool) {
	client, ok := s.poolSvc.GetClientByID(botUserID)
	if !ok {
		return discord.User{}, false
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return discord.User{}, false
	}
	return selfUser.User, true
}

// JoinChannel makes the speaker bot join the given voice channel.
func (s *Service) JoinChannel(ctx context.Context, speakerID, guildID, channelID snowflake.ID) error {
	client, ok := s.poolSvc.GetClientByID(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s is not in the pool", speakerID)
	}

	conn := client.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return fmt.Errorf("speaker %s join channel %s: %w", speakerID, channelID, err)
	}

	slog.Info("speaker joined channel",
		slog.String("speakerID", speakerID.String()),
		slog.String("channelID", channelID.String()),
	)
	return nil
}

// Consume streams audio data from the provided channel to a voice connection and sets up a no-op receiver for incoming frames.
func (s *Service) Consume(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte) error {
	client, ok := s.poolSvc.GetClientByID(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s is not in the pool", speakerID)
	}

	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("speaker %s is not connected to a voice channel in guild %s", speakerID, guildID)
	}
	provider := opus.NewVoiceProvider(chOut)
	conn.SetOpusFrameProvider(provider)
	receiver := opus.NewEmptyVoiceReceiver()
	conn.SetOpusFrameReceiver(receiver)

	go func() {
		<-ctx.Done()
		provider.Close()
		receiver.Close()
	}()

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

	return nil
}

// LeaveChannel makes the speaker bot leave its current voice channel.
func (s *Service) LeaveChannel(ctx context.Context, guildID, speakerID snowflake.ID) {
	client, ok := s.poolSvc.GetClientByID(speakerID)
	if !ok {
		return
	}

	if conn := client.VoiceManager.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}

	slog.Info("speaker left channel", slog.String("speakerID", speakerID.String()))
}
