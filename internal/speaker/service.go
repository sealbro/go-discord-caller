package speaker

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service manages the lifecycle of speaker bot gateway connections and audio relay.
type Service struct {
	mu          sync.RWMutex
	poolClients map[string]*bot.Client       // token -> pre-connected gateway (available pool)
	clients     map[snowflake.ID]*bot.Client // speakerID -> assigned gateway
	cancels     map[snowflake.ID]context.CancelFunc
	store       store.Store
}

// NewService creates a new speaker Service.
func NewService(st store.Store) *Service {
	return &Service{
		poolClients: make(map[string]*bot.Client),
		clients:     make(map[snowflake.ID]*bot.Client),
		cancels:     make(map[snowflake.ID]context.CancelFunc),
		store:       st,
	}
}

// ConnectPool opens a dedicated gateway connection for every token in the pool at startup.
// These gateways sit idle until a speaker is registered via AddNextSpeaker.
func (s *Service) ConnectPool(ctx context.Context, tokens []string) {
	for i, token := range tokens {
		client, err := disgo.New(token,
			bot.WithGatewayConfigOpts(
				gateway.WithIntents(gateway.IntentGuildVoiceStates),
			),
			bot.WithVoiceManagerConfigOpts(
				voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			),
		)
		if err != nil {
			slog.Warn("pool: failed to build gateway",
				slog.Int("index", i+1),
				slog.Any("err", err),
			)
			continue
		}

		if err = client.OpenGateway(ctx); err != nil {
			slog.Warn("pool: failed to open gateway",
				slog.Int("index", i+1),
				slog.Any("err", err),
			)
			client.Close(ctx)
			continue
		}

		s.mu.Lock()
		s.poolClients[token] = client
		s.mu.Unlock()

		slog.Info("pool: speaker gateway ready", slog.Int("index", i+1))
	}
}

// ClosePool shuts down all gateways that are still in the available pool (not yet assigned).
func (s *Service) ClosePool(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for token, client := range s.poolClients {
		client.Close(ctx)
		delete(s.poolClients, token)
	}
	slog.Info("pool: all unassigned gateways closed")
}

// PoolClientUser returns the discord.User of the bot for the given pool token
// by reading the self-user from the pre-connected gateway's cache.
// Returns a zero User and false if the client is not in the pool or the
// self-user is not yet available.
func (s *Service) PoolClientUser(token string) (discord.User, bool) {
	s.mu.RLock()
	client, ok := s.poolClients[token]
	s.mu.RUnlock()
	if !ok {
		return discord.User{}, false
	}
	selfUser, ok := client.Caches.SelfUser()
	if !ok {
		return discord.User{}, false
	}
	return selfUser.User, true
}

// NextPoolClientID returns the Discord ApplicationID for the given pool token.
// It first checks the pre-connected pool gateway; if that gateway is not
// available (startup failure, already assigned, etc.) it falls back to
// decoding the ApplicationID directly from the token string — Discord bot
// tokens embed the bot's user/application ID as raw-base64 in the first
// segment, so no network call is required.
func (s *Service) NextPoolClientID(token string) (snowflake.ID, bool) {
	s.mu.RLock()
	client, ok := s.poolClients[token]
	s.mu.RUnlock()

	if ok {
		return client.ApplicationID, true
	}

	// Fall back: decode the ApplicationID from the token string.
	return clientIDFromToken(token)
}

// clientIDFromToken extracts the Discord ApplicationID (= bot user ID) from a
// raw bot token.  Discord tokens are formatted as
// "<base64(userID)>.<timestamp>.<hmac>", where the first segment is the
// bot's user ID encoded with standard base64 (no padding).
func clientIDFromToken(token string) (snowflake.ID, bool) {
	idx := strings.IndexByte(token, '.')
	if idx <= 0 {
		return 0, false
	}
	data, err := base64.RawStdEncoding.DecodeString(token[:idx])
	if err != nil {
		return 0, false
	}
	id, err := snowflake.Parse(string(data))
	if err != nil {
		return 0, false
	}
	return id, true
}

// Connect assigns an already-open pool gateway to the speaker, or opens a new one if
// the token was not pre-connected (e.g. added manually outside the env pool).
func (s *Service) Connect(ctx context.Context, sp *domain.Speaker) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[sp.ID]; ok {
		return nil // already assigned
	}

	var client *bot.Client

	if poolClient, ok := s.poolClients[sp.BotToken]; ok {
		// Reuse the pre-connected gateway — no extra network round-trip needed.
		client = poolClient
		delete(s.poolClients, sp.BotToken)
		slog.Info("speaker assigned from pool",
			slog.String("speakerID", sp.ID.String()),
			slog.String("username", sp.Username),
		)
	} else {
		// Token was not in the pool — open a new gateway on demand.
		var err error
		client, err = disgo.New(sp.BotToken,
			bot.WithGatewayConfigOpts(
				gateway.WithIntents(gateway.IntentGuildVoiceStates),
			),
			bot.WithVoiceManagerConfigOpts(
				voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			),
		)
		if err != nil {
			return fmt.Errorf("build speaker client %s: %w", sp.ID, err)
		}
		if err = client.OpenGateway(ctx); err != nil {
			return fmt.Errorf("open gateway for speaker %s: %w", sp.ID, err)
		}
		slog.Info("speaker connected (new gateway)",
			slog.String("speakerID", sp.ID.String()),
			slog.String("username", sp.Username),
		)
	}

	s.clients[sp.ID] = client
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

	slog.Info("speaker joined channel",
		slog.String("speakerID", speakerID.String()),
		slog.String("channelID", channelID.String()),
	)
	return nil
}

func (s *Service) Consume(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte) error {
	s.mu.RLock()
	client, ok := s.clients[speakerID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("speaker %s is not connected", speakerID)
	}

	// Start the audio capture goroutine for this speaker.
	relayCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[speakerID] = cancel
	s.mu.Unlock()

	var provider *opus.VoiceProvider
	go func() {
		<-relayCtx.Done()
		if provider != nil {
			provider.Close()
		}
	}()

	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("speaker %s is not connected to a voice channel in guild %s", speakerID, guildID)
	}
	provider = opus.NewVoiceProvider(chOut)
	conn.SetOpusFrameProvider(provider)

	if err := conn.SetSpeaking(relayCtx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

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

// relayPacket writes an Opus packet to all speaker connections except the source.
func (s *Service) relayPacket(ctx context.Context, guildID, excludeSpeakerID snowflake.ID, opus []byte) {
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
