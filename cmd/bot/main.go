package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/sealbro/go-discord-caller/internal/bot"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	shutdownTelemetry, err := telemetry.Setup(context.Background(), cfg.OtelEndpoint)
	if err != nil {
		log.Fatalf("failed to setup telemetry: %v", err)
	}
	defer shutdownTelemetry()

	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	if err := b.Run(); err != nil {
		log.Fatalf("bot error: %v", err)
	}
}
