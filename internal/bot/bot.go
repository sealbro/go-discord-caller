package bot

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Bot wraps the disgo client and all application services.
type Bot struct {
	client     *bot.Client
	manager    *manager.Service
	speakerSvc *speaker.Service
	cfg        *config.Config
}

// New creates and configures a new Bot instance with all services wired together.
func New(cfg *config.Config) (*Bot, error) {
	ctx := context.Background()

	// Infrastructure
	st := store.NewInMemoryStore()
	speakerSvc := speaker.NewService(st)
	managerSvc := manager.NewService(st, speakerSvc, cfg.SpeakerTokens)

	// Open one dedicated gateway per speaker token immediately at startup.
	speakerSvc.ConnectPool(ctx, cfg.SpeakerTokens)
	slog.Info("speaker pool ready", slog.Int("total", len(cfg.SpeakerTokens)))

	// Command router
	r := handler.New()

	// Manager (owner) bot client
	client, err := disgo.New(cfg.OwnerBotToken,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
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

	managerSvc.SetOwnerClient(client)

	// Wire command handlers.
	cmdHandlers := NewCommandHandlers(managerSvc)
	cmdHandlers.Register(r)

	client.AddEventListeners(eventListeners(managerSvc)...)

	// Supply a live Discord membership check so NextSpeakerClientID skips bots
	// that are already in the guild (e.g. invited in a previous session).
	managerSvc.SetMemberChecker(func(guildID, userID snowflake.ID) bool {
		_, err := client.Rest.GetMember(guildID, userID)
		return err == nil
	})

	return &Bot{
		client:     client,
		manager:    managerSvc,
		speakerSvc: speakerSvc,
		cfg:        cfg,
	}, nil
}

// Run opens the owner gateway, registers slash commands, and blocks until an OS signal.
func (b *Bot) Run() error {
	ctx := context.Background()

	if err := b.client.OpenGateway(ctx); err != nil {
		return err
	}
	defer func() {
		b.client.Close(ctx)
		// Shut down any pool gateways that were never assigned to a speaker.
		b.speakerSvc.ClosePool(ctx)
	}()

	// Register slash commands globally (or guild-scoped when GuildID is set).
	guildIDs := guildScope(b.cfg.GuildID)
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

// guildScope returns a slice with one guild ID for dev/scoped registration,
// or an empty slice for global registration.
func guildScope(guildID string) []snowflake.ID {
	if guildID == "" {
		return nil
	}
	id, err := snowflake.Parse(guildID)
	if err != nil {
		return nil
	}
	return []snowflake.ID{id}
}
