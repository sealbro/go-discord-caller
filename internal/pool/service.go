package pool

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// PoolService is the interface for speaker pool operations used by dependent packages.
type PoolService interface {
	GetClientByID(botUserID snowflake.ID) (*bot.Client, bool)
	GetIDs() []snowflake.ID
	Reconnect(ctx context.Context, botUserID snowflake.ID) bool
	Shutdown(ctx context.Context)
}

// Service manages the lifecycle of the pool of speaker gateways.
// poolClients is the single source of truth: it maps bot user ID → client.
// A client is always built from the token (so client.Token is always set),
// even when OpenGateway failed — allowing Reconnect to retry without extra state.
type Service struct {
	mu          sync.RWMutex
	poolClients map[snowflake.ID]*bot.Client
}

// NewService creates a new speaker Service.
func NewService() *Service {
	return &Service{poolClients: make(map[snowflake.ID]*bot.Client)}
}

// newPoolClient builds a disgo client for a speaker bot token.
func newPoolClient(token string) (*bot.Client, error) {
	return disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
		),
	)
}

// ConnectPool pre-connects all gateways in the pool concurrently and waits
// for every goroutine to finish (or the context to expire) before returning.
// Bots whose gateway fails to connect are still recorded (with a nil client)
// so Reconnect can retry later using client.Token.
func (s *Service) ConnectPool(ctx context.Context, tokens []string) {
	type result struct {
		botUserID snowflake.ID
		client    *bot.Client // nil if gateway connection failed; Token field is always set
	}

	results := make([]result, len(tokens))

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

			client, err := newPoolClient(token)
			if err != nil {
				slog.Warn("pool: failed to build client",
					slog.Int("index", index),
					slog.Any("err", err),
				)
				return
			}

			// Always record the bot ID and client (even before gateway opens) so
			// the token is available for reconnection via client.Token.
			results[i] = result{botUserID: botUserID, client: client}

			if err = client.OpenGateway(ctx); err != nil {
				slog.Warn("pool: failed to open gateway",
					slog.Int("index", index),
					slog.Any("err", err),
				)
				// Keep the client in results so the token is preserved; mark as offline.
				results[i].client = client
				return
			}

			slog.Info("pool: speaker gateway ready", slog.Int("index", index))
		}(i, token)
	}
	wg.Wait()

	// Store all valid bots; gateway status is checked separately via GetClientByID.
	s.mu.Lock()
	for _, r := range results {
		if r.botUserID == 0 {
			continue // invalid token
		}
		s.poolClients[r.botUserID] = r.client
	}
	s.mu.Unlock()
}

// Reconnect attempts to open the gateway for a bot whose connection failed.
// It reads the token from the stored client.Token field. If the bot already has
// a connected gateway it is a no-op and returns true.
func (s *Service) Reconnect(ctx context.Context, botUserID snowflake.ID) bool {
	s.mu.RLock()
	client, known := s.poolClients[botUserID]
	s.mu.RUnlock()

	if !known {
		return false // unknown bot
	}

	if client != nil && client.Gateway != nil && client.Gateway.Status().IsConnected() {
		return true // already connected; disgo's internal loop handles future drops
	}

	token := ""
	if client != nil {
		token = client.Token
	}
	if token == "" {
		return false
	}

	newClient, err := newPoolClient(token)
	if err != nil {
		slog.Warn("pool: reconnect failed to build client",
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		return false
	}
	if err = newClient.OpenGateway(ctx); err != nil {
		slog.Warn("pool: reconnect failed to open gateway",
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		return false
	}

	s.mu.Lock()
	s.poolClients[botUserID] = newClient
	s.mu.Unlock()
	slog.Info("pool: reconnected speaker gateway", slog.String("botUserID", botUserID.String()))
	return true
}

// StartWatchdog starts a background goroutine that periodically monitors every
// gateway in the pool. On each tick it:
//   - logs a warning for any gateway whose status is not connected (disgo's
//     internal reconnect loop is already running for those; this gives visibility)
//   - actively calls Reconnect for bots whose gateway failed at startup
func (s *Service) StartWatchdog(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.watchdogCheck(ctx)
			}
		}
	}()
}

func (s *Service) watchdogCheck(ctx context.Context) {
	s.mu.RLock()
	ids := s.sortedIDs()
	s.mu.RUnlock()

	for _, botUserID := range ids {
		s.mu.RLock()
		client := s.poolClients[botUserID]
		s.mu.RUnlock()

		if client == nil || client.Gateway == nil {
			slog.Warn("pool: watchdog detected bot without gateway, attempting reconnect",
				slog.String("botUserID", botUserID.String()),
			)
			reconnectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if s.Reconnect(reconnectCtx, botUserID) {
				slog.Info("pool: watchdog successfully reconnected bot",
					slog.String("botUserID", botUserID.String()),
				)
			}
			cancel()
			continue
		}

		status := client.Gateway.Status()
		if status.IsConnected() {
			continue // healthy
		}
		// Gateway exists but is not connected. Disgo's internal reconnect loop is
		// already running with exponential backoff — log for visibility only.
		slog.Warn("pool: watchdog detected disconnected gateway",
			slog.String("botUserID", botUserID.String()),
			slog.String("status", status.String()),
		)
	}
}

// GetClientByID returns the connected client for the given botUserID.
// Returns false if the bot is unknown or its gateway is not yet connected.
func (s *Service) GetClientByID(botUserID snowflake.ID) (*bot.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.poolClients[botUserID]
	if !ok || client == nil || client.Gateway == nil || !client.Gateway.Status().IsConnected() {
		return nil, false
	}
	return client, true
}

// GetClients returns all connected clients sorted by bot user ID.
func (s *Service) GetClients() []*bot.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make([]*bot.Client, 0, len(s.poolClients))
	for _, id := range s.sortedIDs() {
		if c := s.poolClients[id]; c != nil && c.Gateway != nil && c.Gateway.Status().IsConnected() {
			clients = append(clients, c)
		}
	}
	return clients
}

// GetIDs returns all bot user IDs sorted by value.
// Includes bots whose gateway failed to connect at startup.
func (s *Service) GetIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sortedIDs()
}

// sortedIDs returns map keys sorted by snowflake ID value.
// Must be called with mu held (at least read-locked).
func (s *Service) sortedIDs() []snowflake.ID {
	ids := make([]snowflake.ID, 0, len(s.poolClients))
	for id := range s.poolClients {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Shutdown closes all gateways.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, client := range s.poolClients {
		if client != nil {
			client.Close(ctx)
		}
	}
	s.poolClients = make(map[snowflake.ID]*bot.Client)
	slog.Info("pool service shut down")
}
