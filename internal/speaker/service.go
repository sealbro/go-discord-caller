package speaker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// SpeakerService is the interface for speaker operations used by dependent packages.
type SpeakerService interface {
	GetUserByID(botUserID snowflake.ID) (discord.User, bool)
	JoinChannel(ctx context.Context, speakerID, guildID, channelID snowflake.ID) error
	Consume(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte, chCapture chan<- []byte) error
	LeaveChannel(ctx context.Context, guildID, speakerID snowflake.ID)
}

// Service manages the lifecycle of speaker bot gateway connections and audio relay.
type Service struct {
	poolSvc pool.PoolService
	test    config.TestConfig
}

// NewService creates a new speaker Service.
func NewService(poolSvc pool.PoolService, test config.TestConfig) *Service {
	return &Service{
		poolSvc: poolSvc,
		test:    test,
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

// Consume sets up audio for the speaker's voice connection.
// chOut is the provider channel (frames to play). chCapture, when non-nil,
// receives frames captured from the speaker's channel (for mixing); nil disables capture.
func (s *Service) Consume(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte, chCapture chan<- []byte) error {
	client, ok := s.poolSvc.GetClientByID(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s is not in the pool", speakerID)
	}

	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("speaker %s is not connected to a voice channel in guild %s", speakerID, guildID)
	}

	var provider voice.OpusFrameProvider
	if s.test.IsTestBot(speakerID) {
		fp, err := opus.NewFileVoiceProvider(s.test.FileDCA)
		if err != nil {
			return fmt.Errorf("open dca file: %w", err)
		}
		if chCapture != nil {
			// Tee file audio into chCapture so it feeds the relay mixer.
			// Set chCapture to nil so the block below uses EmptyVoiceReceiver
			// instead of also attaching a VoiceReceiver to the same channel.
			provider = opus.NewTeeProvider(fp, chCapture)
			chCapture = nil
		} else {
			provider = fp
		}
		go func() {
			for range chOut {
			}
		}()
	} else {
		provider = opus.NewVoiceProvider(chOut)
	}

	conn.SetOpusFrameProvider(provider)

	var receiver interface {
		Close()
	}
	if chCapture != nil {
		r := opus.NewVoiceReceiver(chCapture, speakerID, nil)
		conn.SetOpusFrameReceiver(r)
		receiver = r
	} else {
		r := opus.NewEmptyVoiceReceiver()
		conn.SetOpusFrameReceiver(r)
		receiver = r
	}

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
