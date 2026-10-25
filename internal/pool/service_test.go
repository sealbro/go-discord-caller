package pool

import (
	"context"
	"encoding/base64"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// statusGateway is a gateway frozen in one status that records Close.
type statusGateway struct {
	gateway.Gateway
	status  gateway.Status
	latency time.Duration
	closes  atomic.Int32
}

func (g *statusGateway) Status() gateway.Status     { return g.status }
func (g *statusGateway) Latency() time.Duration     { return g.latency }
func (g *statusGateway) Close(context.Context)      { g.closes.Add(1) }
func (g *statusGateway) Open(context.Context) error { return nil }

func clientWithGateway(status gateway.Status, latency time.Duration) (*bot.Client, *statusGateway) {
	gw := &statusGateway{status: status, latency: latency}
	return &bot.Client{Caches: cache.New(), Gateway: gw}, gw
}

// botToken builds a token in Discord's format: the bot user ID, base64
// encoded, then two opaque segments. It is never accepted by Discord.
func botToken(id snowflake.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id.String())) + ".AAAAAA.not-a-real-secret"
}

func newMetricsService(t *testing.T) (*Service, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return NewService(&metrics.Pool, &metrics.Voice, nil, nil), reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// counterByBot sums the named Int64 counter per bot_id; a bot with a series
// at zero is present in the map.
func counterByBot(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	values := map[string]int64{}
	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				id, _ := dp.Attributes.Value("bot_id")
				values[id.AsString()] += dp.Value
			}
		}
	}
	return values
}

const (
	attemptsMetric = "gdc.pool.reconnect.attempts.total"
	failuresMetric = "gdc.pool.reconnect.failures.total"
)

// cancelledCtx makes every gateway open fail before it reaches the network.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestConnectPoolKeepsBotsWhoseGatewayFailed(t *testing.T) {
	t.Parallel()
	const offline snowflake.ID = 701
	s, reader := newMetricsService(t)

	start := time.Now()
	s.ConnectPool(cancelledCtx(), []string{"not-a-token", botToken(offline)})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ConnectPool took %s with a cancelled context", elapsed)
	}

	if got := s.GetIDs(); !slices.Equal(got, []snowflake.ID{offline}) {
		t.Fatalf("GetIDs = %v, want only the bot with a well-formed token", got)
	}
	if _, ok := s.GetClientByID(offline); ok {
		t.Error("GetClientByID returned a bot whose gateway never opened")
	}
	if got := s.GetClients(); len(got) != 0 {
		t.Errorf("GetClients = %d clients, want none connected", len(got))
	}
	if got := s.ConnectedSpeakerIDs(); len(got) != 0 {
		t.Errorf("ConnectedSpeakerIDs = %v, want none", got)
	}
	s.mu.RLock()
	token := s.poolClients[offline].Token
	s.mu.RUnlock()
	if token != botToken(offline) {
		t.Error("offline bot's token was not kept for a later reconnect")
	}
	for _, name := range []string{attemptsMetric, failuresMetric} {
		if got, ok := counterByBot(t, reader, name)[offline.String()]; !ok || got != 0 {
			t.Errorf("%s for the offline bot = %d (present %v), want a seeded 0", name, got, ok)
		}
	}
}

func TestConnectedClientsAreSortedAndFiltered(t *testing.T) {
	t.Parallel()
	s, _ := newMetricsService(t)
	up1, _ := clientWithGateway(gateway.StatusReady, 0)
	up2, _ := clientWithGateway(gateway.StatusReady, 0)
	down, _ := clientWithGateway(gateway.StatusDisconnected, 0)
	s.poolClients[30] = up2
	s.poolClients[10] = up1
	s.poolClients[20] = down
	s.poolClients[40] = nil

	if got := s.GetIDs(); !slices.Equal(got, []snowflake.ID{10, 20, 30, 40}) {
		t.Errorf("GetIDs = %v, want every bot sorted", got)
	}
	if got := s.ConnectedSpeakerIDs(); !slices.Equal(got, []snowflake.ID{10, 30}) {
		t.Errorf("ConnectedSpeakerIDs = %v, want [10 30]", got)
	}
	if got := s.GetClients(); len(got) != 2 || got[0] != up1 || got[1] != up2 {
		t.Errorf("GetClients = %v, want the two connected clients in ID order", got)
	}
	if c, ok := s.GetClientByID(10); !ok || c != up1 {
		t.Error("GetClientByID(10) did not return the connected client")
	}
	for _, id := range []snowflake.ID{20, 40, 99} {
		if _, ok := s.GetClientByID(id); ok {
			t.Errorf("GetClientByID(%s) = ok for a bot that is not connected", id)
		}
	}
}

func TestReconnect(t *testing.T) {
	t.Parallel()
	const bot snowflake.ID = 702

	t.Run("unknown bot", func(t *testing.T) {
		t.Parallel()
		s, _ := newMetricsService(t)
		if s.Reconnect(context.Background(), bot) {
			t.Error("Reconnect = true for a bot outside the pool")
		}
	})
	t.Run("already connected", func(t *testing.T) {
		t.Parallel()
		s, reader := newMetricsService(t)
		client, gw := clientWithGateway(gateway.StatusReady, 0)
		s.poolClients[bot] = client
		if !s.Reconnect(context.Background(), bot) {
			t.Error("Reconnect = false for a connected bot")
		}
		if attempts := counterByBot(t, reader, attemptsMetric)[bot.String()]; attempts != 0 || gw.closes.Load() != 0 {
			t.Errorf("connected bot was reconnected: %d attempts, %d closes", attempts, gw.closes.Load())
		}
	})
	t.Run("no token to reconnect with", func(t *testing.T) {
		t.Parallel()
		s, reader := newMetricsService(t)
		s.poolClients[bot] = nil
		if s.Reconnect(context.Background(), bot) {
			t.Error("Reconnect = true without a token")
		}
		if attempts := counterByBot(t, reader, attemptsMetric)[bot.String()]; attempts != 0 {
			t.Errorf("%d attempts recorded without a token, want 0", attempts)
		}
	})
	t.Run("gateway fails to open", func(t *testing.T) {
		t.Parallel()
		s, reader := newMetricsService(t)
		old, gw := clientWithGateway(gateway.StatusDisconnected, 0)
		old.Token = botToken(bot)
		s.poolClients[bot] = old

		if s.Reconnect(cancelledCtx(), bot) {
			t.Fatal("Reconnect = true although the gateway did not open")
		}
		if got := counterByBot(t, reader, attemptsMetric)[bot.String()]; got != 1 {
			t.Errorf("attempts = %d, want 1", got)
		}
		if got := counterByBot(t, reader, failuresMetric)[bot.String()]; got != 1 {
			t.Errorf("failures = %d, want 1", got)
		}
		s.mu.RLock()
		kept := s.poolClients[bot]
		s.mu.RUnlock()
		if kept != old || gw.closes.Load() != 0 {
			t.Error("a failed reconnect replaced or closed the existing client")
		}
	})
}

// The watchdog reconnects only bots that have no gateway at all; a gateway
// that exists but is down is already being retried by disgo.
func TestWatchdogReconnectsOnlyBotsWithoutAGateway(t *testing.T) {
	t.Parallel()
	const (
		healthy      snowflake.ID = 711
		disconnected snowflake.ID = 712
		noGateway    snowflake.ID = 713
	)
	s, reader := newMetricsService(t)
	s.poolClients[healthy], _ = clientWithGateway(gateway.StatusReady, 0)
	down, _ := clientWithGateway(gateway.StatusDisconnected, 0)
	down.Token = botToken(disconnected)
	s.poolClients[disconnected] = down
	s.poolClients[noGateway] = &bot.Client{Caches: cache.New(), Token: botToken(noGateway)}

	s.watchdogCheck(cancelledCtx())

	attempts := counterByBot(t, reader, attemptsMetric)
	if attempts[noGateway.String()] != 1 {
		t.Errorf("bot without a gateway: %d reconnect attempts, want 1", attempts[noGateway.String()])
	}
	for _, id := range []snowflake.ID{healthy, disconnected} {
		if attempts[id.String()] != 0 {
			t.Errorf("bot %s: %d reconnect attempts, want 0", id, attempts[id.String()])
		}
	}
}

func TestWatchdogStopsWithItsContext(t *testing.T) {
	t.Parallel()
	const noGateway snowflake.ID = 714
	s, reader := newMetricsService(t)
	s.poolClients[noGateway] = &bot.Client{Caches: cache.New(), Token: botToken(noGateway)}

	s.StartWatchdog(cancelledCtx(), time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if got := counterByBot(t, reader, attemptsMetric)[noGateway.String()]; got != 0 {
		t.Errorf("a watchdog with a cancelled context made %d reconnect attempts", got)
	}
}

func TestPoolMetricsReportConnectionState(t *testing.T) {
	t.Parallel()
	const (
		connected    snowflake.ID = 721
		disconnected snowflake.ID = 722
		missing      snowflake.ID = 723
		owner        snowflake.ID = 724
	)
	s, reader := newMetricsService(t)
	up, _ := clientWithGateway(gateway.StatusReady, 120*time.Millisecond)
	up.Caches.SetSelfUser(discord.OAuth2User{User: discord.User{ID: connected, Username: "speaker-up"}})
	s.poolClients[connected] = up
	s.poolClients[disconnected], _ = clientWithGateway(gateway.StatusDisconnected, 0)
	s.poolClients[missing] = nil
	ownerClient, _ := clientWithGateway(gateway.StatusReady, 80*time.Millisecond)
	s.RegisterBot(owner, "owner", ownerClient)
	s.StartMetrics()

	gauges := map[string]int64{}
	latency := map[string]float64{}
	names := map[string]string{}
	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					if m.Name == "gdc.discord.bot" {
						id, _ := dp.Attributes.Value("bot_id")
						name, _ := dp.Attributes.Value("bot_name")
						names[id.AsString()] = name.AsString()
						continue
					}
					gauges[m.Name] = dp.Value
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					id, _ := dp.Attributes.Value("bot_id")
					latency[id.AsString()] = dp.Value
				}
			}
		}
	}

	if gauges["gdc.pool.bots.total"] != 3 || gauges["gdc.pool.bots.connected"] != 1 {
		t.Errorf("pool bots total=%d connected=%d, want 3 and 1",
			gauges["gdc.pool.bots.total"], gauges["gdc.pool.bots.connected"])
	}
	wantLatency := map[string]float64{connected.String(): 120, owner.String(): 80}
	if len(latency) != len(wantLatency) || latency[connected.String()] != 120 || latency[owner.String()] != 80 {
		t.Errorf("gateway latency = %v, want %v (connected bots only)", latency, wantLatency)
	}
	wantNames := map[string]string{
		connected.String(): "speaker-up", disconnected.String(): "", missing.String(): "", owner.String(): "owner",
	}
	if len(names) != len(wantNames) {
		t.Errorf("bot info = %v, want %v", names, wantNames)
	}
	for id, want := range wantNames {
		if got, ok := names[id]; !ok || got != want {
			t.Errorf("bot info for %s = %q (present %v), want %q", id, got, ok, want)
		}
	}
}

func TestShutdownClosesEveryGateway(t *testing.T) {
	t.Parallel()
	s, _ := newMetricsService(t)
	up, upGW := clientWithGateway(gateway.StatusReady, 0)
	down, downGW := clientWithGateway(gateway.StatusDisconnected, 0)
	s.poolClients[1] = up
	s.poolClients[2] = down
	s.poolClients[3] = nil

	s.Shutdown(context.Background())

	if upGW.closes.Load() != 1 || downGW.closes.Load() != 1 {
		t.Errorf("gateway closes: up=%d down=%d, want 1 each", upGW.closes.Load(), downGW.closes.Load())
	}
	if got := s.GetIDs(); len(got) != 0 {
		t.Errorf("GetIDs after shutdown = %v, want none", got)
	}
}
