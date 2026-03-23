package pool

import (
	"context"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service manages the lifecycle of the pool of speaker gateways.
type Service struct {
	mu          sync.RWMutex
	poolClients map[string]*bot.Client // token -> pre-connected gateway (available pool)
	speakers    store.SpeakerStore
}

// NewService creates a new speaker Service.
func NewService(speakers store.SpeakerStore) *Service {
	return &Service{
		poolClients: make(map[string]*bot.Client),
		speakers:    speakers,
	}
}

// ConnectPool pre-connects all gateways in the pool.
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

// GetClientByToken returns the client for the given token, if it exists.
func (s *Service) GetClientByToken(token string) (*bot.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.poolClients[token]
	return client, ok
}

// GetClients returns a slice of all clients in the pool.
func (s *Service) GetClients() []*bot.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make([]*bot.Client, len(s.poolClients))
	i := 0
	for _, client := range s.poolClients {
		clients[i] = client
		i++
	}
	return clients
}

func (s *Service) GetTokens() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tokens := make([]string, len(s.poolClients))
	i := 0
	for token := range s.poolClients {
		tokens[i] = token
		i++
	}
	return tokens
}

// Shutdown closes all gateways and cancels all relay goroutines.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel all relay goroutines and close assigned gateways via the store.
	for _, sp := range s.speakers.ListAllSpeakers() {
		if sp.Cancel != nil {
			sp.Cancel()
			sp.Cancel = nil
		}
	}

	// Close any pool gateways that were never assigned.
	for token, client := range s.poolClients {
		client.Close(ctx)
		delete(s.poolClients, token)
	}
	slog.Info("pool service shut down")
}
