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
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service manages the lifecycle of the pool of speaker gateways.
type Service struct {
	mu          sync.RWMutex
	poolClients map[snowflake.ID]*bot.Client // token -> pre-connected gateway (available pool)
	speakers    store.SpeakerStore
}

// NewService creates a new speaker Service.
func NewService(speakers store.SpeakerStore) *Service {
	return &Service{
		poolClients: make(map[snowflake.ID]*bot.Client),
		speakers:    speakers,
	}
}

// ConnectPool pre-connects all gateways in the pool.
func (s *Service) ConnectPool(ctx context.Context, tokens []string) {
	for i, token := range tokens {
		botUserID, ok := domain.BotUserID(token)
		index := i + 1
		if !ok {
			slog.Warn("pool: invalid pool token", slog.Int("index", index))
			continue
		}

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
				slog.Int("index", index),
				slog.Any("err", err),
			)
			return
		}

		if err = client.OpenGateway(ctx); err != nil {
			slog.Warn("pool: failed to open gateway",
				slog.Int("index", index),
				slog.Any("err", err),
			)
			client.Close(ctx)
			return
		}

		s.mu.Lock()
		s.poolClients[botUserID] = client
		s.mu.Unlock()

		slog.Info("pool: speaker gateway ready", slog.Int("index", index))
	}
}

// GetClientByID returns the client for the given botUserID if it exists.
func (s *Service) GetClientByID(botUserID snowflake.ID) (*bot.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.poolClients[botUserID]
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

func (s *Service) GetIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tokens := make([]snowflake.ID, len(s.poolClients))
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

	// Close any pool gateways that were never assigned.
	for token, client := range s.poolClients {
		client.Close(ctx)
		delete(s.poolClients, token)
	}
	slog.Info("pool service shut down")
}
