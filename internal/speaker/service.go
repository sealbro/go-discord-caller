package speaker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service manages the lifecycle of speaker bot gateway connections and audio relay.
type Service struct {
	mu      sync.RWMutex
	clients map[snowflake.ID]*bot.Client        // speakerID -> gateway client
	cancels map[snowflake.ID]context.CancelFunc // speakerID -> relay goroutine cancel
	store   store.Store
}

// NewService creates a new speaker Service.
func NewService(st store.Store) *Service {
	return &Service{
		clients: make(map[snowflake.ID]*bot.Client),
		cancels: make(map[snowflake.ID]context.CancelFunc),
		store:   st,
	}
}

// Connect opens a Discord gateway connection for the given speaker bot.
func (s *Service) Connect(ctx context.Context, sp *domain.Speaker) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[sp.ID]; ok {
		return nil // already connected
	}

	client, err := disgo.New(sp.BotToken,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildVoiceStates),
		),
	)
	if err != nil {
		return fmt.Errorf("build speaker client %s: %w", sp.ID, err)
	}

	if err = client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("open gateway for speaker %s: %w", sp.ID, err)
	}

	s.clients[sp.ID] = client
	slog.Info("speaker connected", slog.String("speakerID", sp.ID.String()), slog.String("username", sp.Username))
	return nil
}

// Disconnect closes the gateway connection for the given speaker.
func (s *Service) Disconnect(ctx context.Context, speakerID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cancel, ok := s.cancels[speakerID]; ok {
		cancel()
		delete(s.cancels, speakerID)
	}

	if client, ok := s.clients[speakerID]; ok {
		client.Close(ctx)
		delete(s.clients, speakerID)
		slog.Info("speaker disconnected", slog.String("speakerID", speakerID.String()))
	}
}

// JoinChannel makes the speaker bot join the given voice channel and starts audio relay.
func (s *Service) JoinChannel(ctx context.Context, speakerID, guildID, channelID snowflake.ID) error {
	s.mu.RLock()
	client, ok := s.clients[speakerID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("speaker %s is not connected", speakerID)
	}

	conn := client.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return fmt.Errorf("speaker %s join channel %s: %w", speakerID, channelID, err)
	}

	// Start the audio capture goroutine for this speaker.
	relayCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[speakerID] = cancel
	s.mu.Unlock()

	go s.captureAndRelay(relayCtx, speakerID, guildID, client)

	slog.Info("speaker joined channel",
		slog.String("speakerID", speakerID.String()),
		slog.String("channelID", channelID.String()),
	)
	return nil
}

// LeaveChannel makes the speaker bot leave its current voice channel.
func (s *Service) LeaveChannel(ctx context.Context, speakerID, guildID snowflake.ID) {
	s.mu.Lock()
	if cancel, ok := s.cancels[speakerID]; ok {
		cancel()
		delete(s.cancels, speakerID)
	}
	client, ok := s.clients[speakerID]
	s.mu.Unlock()

	if !ok {
		return
	}

	if conn := client.VoiceManager.GetConn(guildID); conn != nil {
		conn.Close(ctx)
	}

	slog.Info("speaker left channel", slog.String("speakerID", speakerID.String()))
}

// captureAndRelay reads Opus packets from the speaker's voice connection and
// forwards them to every other active speaker in the same guild.
func (s *Service) captureAndRelay(ctx context.Context, sourceSpeakerID, guildID snowflake.ID, client *bot.Client) {
	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		packet, err := conn.UDP().ReadPacket()
		if err != nil {
			slog.Debug("audio read stopped",
				slog.String("speakerID", sourceSpeakerID.String()),
				slog.Any("err", err),
			)
			return
		}

		// Only relay audio from users with the bound role.
		// TODO: filter by role — requires resolving SSRC -> userID -> guild member roles.

		s.relayPacket(guildID, sourceSpeakerID, packet.Opus)
	}
}

// relayPacket writes an Opus packet to all speaker connections except the source.
func (s *Service) relayPacket(guildID, excludeSpeakerID snowflake.ID, opus []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for id, client := range s.clients {
		if id == excludeSpeakerID {
			continue
		}
		conn := client.VoiceManager.GetConn(guildID)
		if conn == nil {
			continue
		}
		if _, err := conn.UDP().Write(opus); err != nil {
			slog.Warn("relay write failed",
				slog.String("targetSpeakerID", id.String()),
				slog.Any("err", err),
			)
		}
	}
}
