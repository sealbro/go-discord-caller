package pool

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
)

// openGateway is a gateway whose Open can fail or block, and which records
// how often it was opened and closed.
type openGateway struct {
	gateway.Gateway
	mu      sync.Mutex
	status  gateway.Status
	openErr error
	release chan struct{}
	opens   atomic.Int32
	closes  atomic.Int32
}

func (g *openGateway) Status() gateway.Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

func (g *openGateway) Close(context.Context) { g.closes.Add(1) }

func (g *openGateway) Open(context.Context) error {
	g.opens.Add(1)
	if g.release != nil {
		<-g.release
	}
	if g.openErr != nil {
		return g.openErr
	}
	g.mu.Lock()
	g.status = gateway.StatusReady
	g.mu.Unlock()
	return nil
}

// clientFactory replaces the service's client builder and keeps every client it built.
type clientFactory struct {
	mu      sync.Mutex
	openErr error
	release chan struct{}
	built   []*openGateway
}

func (f *clientFactory) build(token string) (*bot.Client, error) {
	gw := &openGateway{status: gateway.StatusDisconnected, openErr: f.openErr, release: f.release}
	f.mu.Lock()
	f.built = append(f.built, gw)
	f.mu.Unlock()
	return &bot.Client{Caches: cache.New(), Gateway: gw, Token: token}, nil
}

func (f *clientFactory) builtCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.built)
}

func TestReconnectClosesTheClientWhenItsGatewayFailsToOpen(t *testing.T) {
	t.Parallel()
	const id snowflake.ID = 731
	s, _ := newMetricsService(t)
	f := &clientFactory{openErr: errors.New("gateway unreachable")}
	s.newClient = f.build
	old, _ := clientWithGateway(gateway.StatusDisconnected, 0)
	old.Token = botToken(id)
	s.poolClients[id] = old

	if s.Reconnect(context.Background(), id) {
		t.Fatal("Reconnect = true although the gateway did not open")
	}

	if f.builtCount() != 1 {
		t.Fatalf("built %d clients, want 1", f.builtCount())
	}
	if got := f.built[0].closes.Load(); got != 1 {
		t.Errorf("failed attempt's client closed %d times, want 1 (leaked)", got)
	}
}

func TestConcurrentReconnectsOpenOneGateway(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const id snowflake.ID = 732
		s, _ := newMetricsService(t)
		f := &clientFactory{release: make(chan struct{})}
		s.newClient = f.build
		old, _ := clientWithGateway(gateway.StatusDisconnected, 0)
		old.Token = botToken(id)
		s.poolClients[id] = old
		release := sync.OnceFunc(func() { close(f.release) })
		t.Cleanup(release)

		results := make(chan bool, 2)
		for range 2 {
			go func() { results <- s.Reconnect(context.Background(), id) }()
		}
		synctest.Wait()
		if len(results) != 0 {
			t.Fatal("precondition failed: a Reconnect returned while the gateway open was still blocked")
		}
		if got := f.builtCount(); got != 1 {
			t.Fatalf("built %d clients while two reconnects were in flight together, want 1 (second IDENTIFY)", got)
		}
		release()

		for range 2 {
			if !<-results {
				t.Error("a concurrent Reconnect returned false")
			}
		}
		if got := f.builtCount(); got != 1 {
			t.Errorf("built %d clients for two concurrent reconnects, want 1 (second IDENTIFY)", got)
		}
		for i, gw := range f.built {
			if gw.opens.Load() != 1 {
				t.Errorf("client %d opened %d times, want 1", i, gw.opens.Load())
			}
			if gw.closes.Load() != 0 {
				t.Errorf("client %d was closed although it is the live one", i)
			}
		}
	})
}

func TestConnectPoolSkipsDuplicateTokens(t *testing.T) {
	t.Parallel()
	const id snowflake.ID = 733
	s, _ := newMetricsService(t)
	f := &clientFactory{}
	s.newClient = f.build

	s.ConnectPool(context.Background(), []string{botToken(id), botToken(id)})

	if got := s.GetIDs(); !slices.Equal(got, []snowflake.ID{id}) {
		t.Fatalf("GetIDs = %v, want one entry", got)
	}
	if got := f.builtCount(); got != 1 {
		t.Errorf("built %d clients for two identical tokens, want 1 (first would leak)", got)
	}
}

func TestWatchdogReplacesAClientThatStaysDisconnectedPastTheGrace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const id snowflake.ID = 734
		s, reader := newMetricsService(t)
		f := &clientFactory{}
		s.newClient = f.build
		dead, _ := clientWithGateway(gateway.StatusDisconnected, 0)
		dead.Token = botToken(id)
		s.poolClients[id] = dead

		s.watchdogCheck(context.Background())
		time.Sleep(s.downGrace - time.Nanosecond)
		s.watchdogCheck(context.Background())
		if got := counterByBot(t, reader, attemptsMetric)[id.String()]; got != 0 {
			t.Fatalf("reconnected inside the %v grace period (%d attempts)", s.downGrace, got)
		}

		time.Sleep(time.Nanosecond)
		s.watchdogCheck(context.Background())
		if got := counterByBot(t, reader, attemptsMetric)[id.String()]; got != 1 {
			t.Fatalf("bot stuck disconnected for the %v grace: %d reconnect attempts, want 1", s.downGrace, got)
		}
		if _, ok := s.GetClientByID(id); !ok {
			t.Error("bot is still not connected after the watchdog reconnected it")
		}
	})
}

func TestWatchdogDoesNotRetryEveryTick(t *testing.T) {
	t.Parallel()
	const id snowflake.ID = 735
	s, reader := newMetricsService(t)
	f := &clientFactory{openErr: errors.New("gateway unreachable")}
	s.newClient = f.build
	s.downGrace = 0
	s.retryInterval = time.Hour
	dead, _ := clientWithGateway(gateway.StatusDisconnected, 0)
	dead.Token = botToken(id)
	s.poolClients[id] = dead

	for range 3 {
		s.watchdogCheck(context.Background())
	}
	if got := counterByBot(t, reader, attemptsMetric)[id.String()]; got != 1 {
		t.Errorf("%d reconnect attempts in 3 ticks inside the retry interval, want 1", got)
	}
}
