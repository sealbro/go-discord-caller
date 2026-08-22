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
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric"
)

// PoolService is the interface for speaker pool operations used by dependent packages.
type PoolService interface {
	GetClientByID(botUserID snowflake.ID) (*bot.Client, bool)
	GetIDs() []snowflake.ID
	Reconnect(ctx context.Context, botUserID snowflake.ID) bool
	Shutdown(ctx context.Context)
}

// Service manages the lifecycle of the pool of speaker bot gateways.
// poolClients maps bot user ID → client for speaker bots only.
// extraBots holds bots (e.g. the owner bot) that are tracked for the info and
// gateway-latency metrics but not managed by the pool lifecycle.
type Service struct {
	mu          sync.RWMutex
	poolClients map[snowflake.ID]*bot.Client
	extraBots   map[snowflake.ID]extraBot // id → bot tracked for metrics only
	metrics     *telemetry.PoolMetrics
}

// extraBot is a bot reported in the info/latency metrics but not lifecycle-managed
// by the pool (e.g. the owner bot). client may be nil (name-only registration).
type extraBot struct {
	name   string
	client *bot.Client
}

// NewService creates a new speaker Service.
func NewService(metrics *telemetry.PoolMetrics) *Service {
	return &Service{
		poolClients: make(map[snowflake.ID]*bot.Client),
		extraBots:   make(map[snowflake.ID]extraBot),
		metrics:     metrics,
	}
}

// RegisterBot adds a bot to the info and gateway-latency metrics that is not part
// of the speaker pool (e.g. the owner bot). Pass the bot's client so its gateway
// heartbeat RTT is reported alongside the pool bots; client may be nil to record
// the name only. Safe to call concurrently.
func (s *Service) RegisterBot(id snowflake.ID, name string, client *bot.Client) {
	s.mu.Lock()
	s.extraBots[id] = extraBot{name: name, client: client}
	s.mu.Unlock()
}

// newPoolClient builds a disgo client for a speaker bot token.
func newPoolClient(token string) (*bot.Client, error) {
	return disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			voice.WithLogger(telemetry.VoiceLogger()),
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

			botUserID, ok := guild.BotUserID(token)
			if !ok {
				slog.WarnContext(ctx, "pool: invalid pool token", slog.Int("index", index))
				return
			}

			client, err := newPoolClient(token)
			if err != nil {
				slog.WarnContext(ctx, "pool: failed to build client",
					slog.Int("index", index),
					slog.Any("err", err),
				)
				return
			}

			// Always record the bot ID and client (even before gateway opens) so
			// the token is available for reconnection via client.Token.
			results[i] = result{botUserID: botUserID, client: client}

			if err = client.OpenGateway(ctx); err != nil {
				slog.WarnContext(ctx, "pool: failed to open gateway",
					slog.Int("index", index),
					slog.Any("err", err),
				)
				// Keep the client in results so the token is preserved; mark as offline.
				results[i].client = client
				return
			}

			slog.InfoContext(ctx, "pool: speaker gateway ready", slog.Int("index", index))
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
		// Seed a zero data point so the reconnect-attempts/failures series exist
		// (and the dashboard panels show 0 instead of "No data") even for bots
		// that never need a reconnect.
		s.metrics.SeedReconnectSeries(ctx, r.botUserID)
	}
	s.mu.Unlock()
}

// StartMetrics registers OTel observable callbacks for pool health gauges.
// Call once after the pool is connected, alongside StartWatchdog.
func (s *Service) StartMetrics() {
	if err := s.metrics.RegisterObservers(s.observePoolBots); err != nil {
		slog.Error("pool: failed to register pool metrics callback", slog.Any("err", err))
	}
}

func (s *Service) observePoolBots(_ context.Context, o metric.Observer) error {
	s.mu.RLock()
	total := int64(len(s.poolClients))
	connected := int64(0)
	for id, c := range s.poolClients {
		botName := ""
		if c != nil {
			if self, ok := c.Caches.SelfUser(); ok {
				botName = self.Username
			}
		}
		s.metrics.ObserveBotInfo(o, id.String(), botName)

		if c == nil || !isConnected(c) {
			continue
		}
		connected++
		// Emit per-bot gateway heartbeat RTT.
		// Latency() returns 0 until the first heartbeat ACK is received.
		latMs := float64(c.Gateway.Latency().Milliseconds())
		s.metrics.ObserveGatewayLatency(o, id.String(), latMs)
	}
	for id, eb := range s.extraBots {
		s.metrics.ObserveBotInfo(o, id.String(), eb.name)
		// Report the owner bot's gateway RTT too, so it appears in the latency
		// panels alongside the pool bots (it is not part of poolClients).
		if eb.client == nil || !isConnected(eb.client) {
			continue
		}
		latMs := float64(eb.client.Gateway.Latency().Milliseconds())
		s.metrics.ObserveGatewayLatency(o, id.String(), latMs)
	}
	s.mu.RUnlock()
	s.metrics.ObservePoolBots(o, total, connected)
	return nil
}

// Reconnect attempts to open the gateway for a bot whose connection failed.
// It reads the token from the stored client.Token field. If the bot already has
// a connected gateway it is a no-op and returns true.
func (s *Service) Reconnect(ctx context.Context, botUserID snowflake.ID) bool {
	s.mu.RLock()
	client, known := s.poolClients[botUserID]
	s.mu.RUnlock()

	if !known {
		return false
	}

	if isConnected(client) {
		return true // already connected; disgo's internal loop handles future drops
	}

	token := ""
	if client != nil {
		token = client.Token
	}
	if token == "" {
		return false
	}

	s.metrics.ReconnectAttempt(ctx, botUserID)

	newClient, err := newPoolClient(token)
	if err != nil {
		slog.WarnContext(ctx, "pool: reconnect failed to build client",
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		s.metrics.ReconnectFailed(ctx, botUserID)
		return false
	}
	if err = newClient.OpenGateway(ctx); err != nil {
		slog.WarnContext(ctx, "pool: reconnect failed to open gateway",
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		s.metrics.ReconnectFailed(ctx, botUserID)
		return false
	}

	// Swap the map entry under mu, then close the old client OUTSIDE the lock.
	// Gateway shutdown can take seconds; holding s.mu across it would block
	// every other reconnect / GetClientByID / observer callback in the
	// meantime. The old client's internal goroutines and WebSocket connection
	// are released by Close — and any voice connections the manager had on
	// it become unreachable (they were already dead from the manager's
	// perspective once isConnected returned false, which is the precondition
	// that brought us here).
	s.mu.Lock()
	old := s.poolClients[botUserID]
	s.poolClients[botUserID] = newClient
	s.mu.Unlock()
	if old != nil {
		old.Close(ctx)
	}
	slog.InfoContext(ctx, "pool: reconnected speaker gateway", slog.String("botUserID", botUserID.String()))
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

		if isConnected(client) {
			continue // healthy
		}

		if client == nil || client.Gateway == nil {
			slog.WarnContext(ctx, "pool: watchdog detected bot without gateway, attempting reconnect",
				slog.String("botUserID", botUserID.String()),
			)
			reconnectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if s.Reconnect(reconnectCtx, botUserID) {
				slog.InfoContext(ctx, "pool: watchdog successfully reconnected bot",
					slog.String("botUserID", botUserID.String()),
				)
			}
			cancel()
			continue
		}

		// Gateway exists but is not connected. Disgo's internal reconnect loop is
		// already running with exponential backoff — log for visibility only.
		slog.WarnContext(ctx, "pool: watchdog detected disconnected gateway",
			slog.String("botUserID", botUserID.String()),
			slog.String("status", client.Gateway.Status().String()),
		)
	}
}

// isConnected reports whether a client has a healthy gateway connection.
func isConnected(c *bot.Client) bool {
	return c != nil && c.Gateway != nil && c.Gateway.Status().IsConnected()
}

// GetClientByID returns the connected client for the given botUserID.
// Returns false if the bot is unknown or its gateway is not yet connected.
func (s *Service) GetClientByID(botUserID snowflake.ID) (*bot.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.poolClients[botUserID]
	if !ok || !isConnected(client) {
		return nil, false
	}
	return client, true
}

// GetClients returns all connected speaker clients sorted by bot user ID.
func (s *Service) GetClients() []*bot.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make([]*bot.Client, 0, len(s.poolClients))
	for _, id := range s.sortedIDs() {
		if c := s.poolClients[id]; isConnected(c) {
			clients = append(clients, c)
		}
	}
	return clients
}

// GetIDs returns all speaker bot user IDs sorted by value.
// Includes speakers whose gateway failed to connect at startup.
func (s *Service) GetIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sortedIDs()
}

// ConnectedSpeakerIDs returns IDs of speakers whose gateway is currently
// connected, sorted by snowflake value. Dead or unconnected speakers are
// excluded.
func (s *Service) ConnectedSpeakerIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []snowflake.ID
	for _, id := range s.sortedIDs() {
		if isConnected(s.poolClients[id]) {
			ids = append(ids, id)
		}
	}
	return ids
}

// sortedIDs returns pool client IDs sorted by snowflake value.
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
	slog.InfoContext(ctx, "pool service shut down")
}
