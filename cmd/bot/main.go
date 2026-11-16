package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sealbro/go-discord-caller/internal/bot"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel"
)

var errRunFailed = errors.New("bot failed")

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if run(cfg) != nil {
		os.Exit(1)
	}
}

// run logs each failure itself, while the telemetry providers are still open,
// so the crash reason reaches the log pipeline before the deferred shutdown.
func run(cfg *config.Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.OtelEndpoint, cfg.LogLevel)
	if err != nil {
		return failed("failed to setup telemetry", err)
	}
	defer shutdownTelemetry()

	stopProfiling, err := telemetry.StartProfiling(cfg.PyroscopeEndpoint)
	if err != nil {
		return failed("failed to start profiling", err)
	}
	defer stopProfiling()

	telemetry.SetVoiceLogLevel(cfg.VoiceLogLevel)

	st, err := store.NewYAMLStore(cfg.StorePath)
	if err != nil {
		return failed("failed to open store", err, slog.String("path", cfg.StorePath))
	}

	b, err := bot.New(cfg, st, otel.Meter(telemetry.ServiceName))
	if err != nil {
		return failed("failed to create bot", err)
	}

	if err := b.Run(ctx); err != nil {
		return failed("bot error", err)
	}
	return nil
}

func failed(msg string, err error, attrs ...any) error {
	slog.Error(msg, append(attrs, slog.Any("err", err))...)
	return errRunFailed
}
