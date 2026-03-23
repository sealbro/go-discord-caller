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
)

// Service manages the lifecycle of the pool of speaker gateways.
type Service struct {
	mu          sync.RWMutex
	poolClients map[snowflake.ID]*bot.Client // botUserID -> pre-connected gateway
	ids         []snowflake.ID               // ordered; preserves token file order for NextSpeakerID
}

// NewService creates a new speaker Service.
func NewService() *Service {
	return &Service{
		poolClients: make(map[snowflake.ID]*bot.Client),
	}
}

// ConnectPool pre-connects all gateways in the pool concurrently and waits
// for every goroutine to finish (or the context to expire) before returning.
func (s *Service) ConnectPool(ctx context.Context, tokens []string) {
	type result struct {
		index     int
		botUserID snowflake.ID
		client    *bot.Client
	}

	results := make([]result, len(tokens)) // pre-allocated; index is the slot

	var wg sync.WaitGroup
	wg.Add(len(tokens))
	for i, token := range tokens {
		go func(i int, token string) {
			defer wg.Done()
			index := i + 1

			botUserID, ok := domain.BotUserID(token)
			if !ok {
				slog.Warn("pool: invalid pool token", slog.Int("index", index))
				return
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

			slog.Info("pool: speaker gateway ready", slog.Int("index", index))
			results[i] = result{index: index, botUserID: botUserID, client: client}
		}(i, token)
	}
	wg.Wait()

	// Merge results in token order so ids preserves the original ordering.
	s.mu.Lock()
	for _, r := range results {
		if r.client == nil {
			continue
		}
		s.poolClients[r.botUserID] = r.client
		s.ids = append(s.ids, r.botUserID)
	}
	s.mu.Unlock()
}

// GetClientByID returns the client for the given botUserID if it exists.
func (s *Service) GetClientByID(botUserID snowflake.ID) (*bot.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.poolClients[botUserID]
	return client, ok
}

// GetClients returns a slice of all clients in the pool in insertion order.
func (s *Service) GetClients() []*bot.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make([]*bot.Client, 0, len(s.ids))
	for _, id := range s.ids {
		if c, ok := s.poolClients[id]; ok {
			clients = append(clients, c)
		}
	}
	return clients
}

// GetIDs returns bot user IDs in the order their tokens were supplied.
func (s *Service) GetIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]snowflake.ID, len(s.ids))
	copy(result, s.ids)
	return result
}

// Shutdown closes all gateways and cancels all relay goroutines.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range s.ids {
		if client, ok := s.poolClients[id]; ok {
			client.Close(ctx)
			delete(s.poolClients, id)
		}
	}
	s.ids = nil
	slog.Info("pool service shut down")
}
