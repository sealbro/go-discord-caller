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
// Client and Cancel are stored directly on each domain.Speaker.
type Service struct {
	mu          sync.RWMutex
	poolClients map[string]*bot.Client // token -> pre-connected gateway (available pool)
	store       store.Store
}

// NewService creates a new speaker Service.
func NewService(st store.Store) *Service {
	return &Service{
		poolClients: make(map[string]*bot.Client),
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

// Shutdown cancels every active audio relay, closes all assigned speaker
// gateways, and closes any remaining pool gateways.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel all relay goroutines and close assigned gateways via the store.
	for _, sp := range s.store.ListAllSpeakers() {
		if sp.Cancel != nil {
			sp.Cancel()
			sp.Cancel = nil
		}
		if sp.Client != nil {
			sp.Client.Close(ctx)
			sp.Client = nil
			slog.Info("speaker gateway closed", slog.String("speakerID", sp.ID.String()))
		}
	}

	// Close any pool gateways that were never assigned.
	for token, client := range s.poolClients {
		client.Close(ctx)
		delete(s.poolClients, token)
	}
	slog.Info("speaker service shut down")
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

// Connect assigns an already-open pool gateway to the speaker, or opens a new one.
// The client is stored directly on sp.Client.
func (s *Service) Connect(ctx context.Context, sp *domain.Speaker) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sp.Client != nil {
		return nil // already assigned
	}

	var client *bot.Client

	if poolClient, ok := s.poolClients[sp.BotToken]; ok {
		client = poolClient
		delete(s.poolClients, sp.BotToken)
		slog.Info("speaker assigned from pool",
			slog.String("speakerID", sp.ID.String()),
			slog.String("username", sp.Username),
		)
	} else {
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

	sp.Client = client
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
