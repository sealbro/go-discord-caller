package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sealbro/go-discord-caller/internal/bot"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     cfg.LogLevel,
	})))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.OtelEndpoint, cfg.LogLevel)
	if err != nil {
		log.Fatalf("failed to setup telemetry: %v", err)
	}
	defer shutdownTelemetry()

	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	if err := b.Run(ctx); err != nil {
		log.Fatalf("bot error: %v", err)
	}
}
