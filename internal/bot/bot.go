package bot

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Bot wraps the disgo client and all application services.
type Bot struct {
	client  *bot.Client
	manager *manager.Service
	cfg     *config.Config
}

// New creates and configures a new Bot instance with all services wired together.
func New(cfg *config.Config) (*Bot, error) {
	ctx := context.Background()

	// Command router
	r := handler.New()

	// Manager (owner) bot client
	client, err := disgo.New(cfg.OwnerBotToken,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMembers,
				gateway.IntentGuildVoiceStates,
				gateway.IntentGuildMessages,
			),
		),
		bot.WithEventListeners(r),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
		),
	)
	if err != nil {
		return nil, err
	}

	// Infrastructure
	st := store.NewInMemoryStore()
	poolSvc := pool.NewService(st)
	speakerSvc := speaker.NewService(st, poolSvc)
	managerSvc := manager.NewService(st, speakerSvc, poolSvc, client)

	// Open one dedicated gateway per speaker token immediately at startup.
	poolSvc.ConnectPool(ctx, cfg.SpeakerTokens)
	slog.Info("speaker pool ready", slog.Int("total", len(cfg.SpeakerTokens)))

	// Wire command handlers.
	cmdHandlers := NewCommandHandlers(managerSvc)
	cmdHandlers.Register(r)

	client.AddEventListeners(eventListeners(managerSvc)...)

	return &Bot{
		client:  client,
		manager: managerSvc,
		cfg:     cfg,
	}, nil
}

// Run opens the owner gateway, registers slash commands, and blocks until an OS signal.
func (b *Bot) Run() error {
	ctx := context.Background()

	if err := b.client.OpenGateway(ctx); err != nil {
		return err
	}
	defer func() {
		// Graceful shutdown: stop all raids, close all speaker gateways, then the owner gateway.
		b.manager.Shutdown(ctx)
		b.client.Close(ctx)
	}()

	// Register slash commands scoped to every guild the bot is already in.
	guildIDs := b.discoverGuildIDs(5 * time.Second)
	slog.Info("discovered guilds for command sync", slog.Int("count", len(guildIDs)))
	if err := handler.SyncCommands(b.client, Commands, guildIDs); err != nil {
		slog.Warn("failed to sync slash commands", slog.Any("err", err))
	}

	slog.Info("bot is running. Press Ctrl+C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down...")

	return nil
}

// discoverGuildIDs waits (up to timeout) for all GUILD_CREATE events to be
// processed by the cache, then returns the IDs of every guild the bot is in.
// If the bot is not in any guild, nil is returned (global command registration).
func (b *Bot) discoverGuildIDs(timeout time.Duration) []snowflake.ID {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(b.client.Caches.UnreadyGuildIDs()) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var ids []snowflake.ID
	for g := range b.client.Caches.Guilds() {
		ids = append(ids, g.ID)
	}
	return ids
}
