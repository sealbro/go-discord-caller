package telemetry

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	olog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// restoreGlobals puts back everything Setup replaces process-wide.
func restoreGlobals(t *testing.T) {
	t.Helper()
	logger := slog.Default()
	tp, mp, lp := otel.GetTracerProvider(), otel.GetMeterProvider(), olog.GetLoggerProvider()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		olog.SetLoggerProvider(lp)
	})
}

// collector is an in-process gRPC server that records which OTLP services
// were called and answers every call as unimplemented.
type collector struct {
	addr string
	mu   sync.Mutex
	hits map[string]int
}

func startCollector(t *testing.T) *collector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c := &collector{addr: lis.Addr().String(), hits: make(map[string]int)}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		c.mu.Lock()
		c.hits[method]++
		c.mu.Unlock()
		return status.Error(codes.Unimplemented, "test collector")
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return c
}

func (c *collector) called(service string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for method, n := range c.hits {
		if strings.Contains(method, service) && n > 0 {
			return true
		}
	}
	return false
}

func TestSetupWithoutEndpointLogsLocallyAtTheLevel(t *testing.T) {
	restoreGlobals(t)
	mp := otel.GetMeterProvider()

	shutdown, err := Setup(t.Context(), "", slog.LevelWarn)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown()

	if slog.Default().Enabled(t.Context(), slog.LevelInfo) {
		t.Error("Info is enabled at LevelWarn")
	}
	if !slog.Default().Enabled(t.Context(), slog.LevelWarn) {
		t.Error("Warn is disabled at LevelWarn")
	}
	if otel.GetMeterProvider() != mp {
		t.Error("Setup without an endpoint replaced the global meter provider")
	}
}

// Shutdown is the last chance to flush, so it must reach the collector, and do
// so well within its budget when the collector answers.
func TestSetupExportsToTheEndpointOnShutdown(t *testing.T) {
	restoreGlobals(t)
	c := startCollector(t)

	shutdown, err := Setup(t.Context(), c.addr, slog.LevelWarn)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, ok := otel.GetMeterProvider().(*metric.MeterProvider); !ok {
		t.Errorf("global meter provider is %T, want the SDK provider", otel.GetMeterProvider())
	}
	if slog.Default().Enabled(t.Context(), slog.LevelInfo) {
		t.Error("Info is enabled at LevelWarn")
	}

	slog.Warn("flushed on shutdown")
	start := time.Now()
	shutdown()
	if took := time.Since(start); took >= shutdownTimeout {
		t.Errorf("shutdown took %v against an answering collector, the whole per-step budget", took)
	}

	for _, service := range []string{"MetricsService", "LogsService"} {
		if !c.called(service) {
			t.Errorf("shutdown never called the collector's %s", service)
		}
	}
}

func TestCloseConnClosesTheConnection(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := closeConn(conn)(context.Background()); err != nil {
		t.Fatalf("closeConn: %v", err)
	}
	if err := conn.Close(); err == nil {
		t.Error("connection still open after closeConn")
	}
}
