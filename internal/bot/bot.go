package bot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// ManagerService is the interface consumed by the bot layer (commands, handlers).
// Defining it here keeps the bot package decoupled from the concrete manager.Service.
type ManagerService interface {
	GetStatus(guildID snowflake.ID) domain.GuildStatus
	HasActiveSession(guildID snowflake.ID) bool
	StartVoiceRaid(ctx context.Context, cancelFunc context.CancelFunc, guildID snowflake.ID) error
	StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error
	BindCallerRole(guildID, roleID snowflake.ID)
	BindManagerRole(guildID, roleID snowflake.ID)
	BindChannel(guildID, userID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)
	BindOwnerChannel(guildID, channelID snowflake.ID)
	UnbindOwnerChannel(guildID snowflake.ID)
	GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool)
	HasManagerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool
	ToggleSpeaker(guildID, speakerID snowflake.ID, enabled bool) error
	NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool)
	HasAvailableToken(guildID snowflake.ID) bool
	SeedExistingSpeakers(guildIDs []snowflake.ID)
	TrySeedMember(guildID, newUserID snowflake.ID)
	RemoveSpeaker(guildID, userID snowflake.ID)
	Shutdown(ctx context.Context)
}

// Bot wraps the disgo client and all application services.
type Bot struct {
	client       *bot.Client
	manager      ManagerService
	cfg          *config.Config
	memberCache  *groupedCache[discord.Member]
	guildReadyCh chan []snowflake.ID
}

// New creates and configures a new Bot instance with all services wired together.
func New(cfg *config.Config) (*Bot, error) {
	ctx := context.Background()

	// Command router
	r := handler.New()

	memberCache := newGroupedCache[discord.Member](5 * time.Minute)

	// Buffered channel (cap 1) receives guild IDs from the Ready event for command sync.
	guildReadyCh := make(chan []snowflake.ID, 1)

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
		bot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagsAll),
			cache.WithMemberCache(cache.NewMemberCache(memberCache)),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
		),
	)
	if err != nil {
		return nil, err
	}

	// Capture guild IDs from the Ready event for use in command sync.
	client.AddEventListeners(bot.NewListenerFunc(func(e *events.Ready) {
		ids := make([]snowflake.ID, 0, len(e.Guilds))
		for _, g := range e.Guilds {
			ids = append(ids, g.ID)
		}
		select {
		case guildReadyCh <- ids:
		default:
		}
	}))

	// Infrastructure
	st, err := store.NewYAMLStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open yaml store %q: %w", cfg.StorePath, err)
	}
	poolSvc := pool.NewService()
	speakerSvc := speaker.NewService(poolSvc)
	managerSvc := manager.NewService(st, speakerSvc, poolSvc, client)

	// Open one dedicated gateway per speaker token immediately at startup.
	poolCtx, poolCancel := context.WithTimeout(ctx, 30*time.Second)
	poolSvc.ConnectPool(poolCtx, cfg.SpeakerTokens)
	poolCancel()
	slog.Info("speaker pool ready", slog.Int("total", len(cfg.SpeakerTokens)))

	// Wire command handlers.
	cmdHandlers := NewCommandHandlers(managerSvc)
	cmdHandlers.Register(r)

	client.AddEventListeners(eventListeners(managerSvc)...)

	return &Bot{
		client:       client,
		manager:      managerSvc,
		cfg:          cfg,
		memberCache:  memberCache,
		guildReadyCh: guildReadyCh,
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
		b.memberCache.Stop()
		b.manager.Shutdown(ctx)
		b.client.Close(ctx)
	}()

	// Wait for the Ready event to deliver guild IDs, then sync slash commands.
	// Falls back to global sync on timeout.
	var guildIDs []snowflake.ID
	select {
	case guildIDs = <-b.guildReadyCh:
	case <-time.After(10 * time.Second):
		slog.Warn("timed out waiting for Ready event, syncing commands globally")
	}
	slog.Info("discovered guilds for command sync", slog.Int("count", len(guildIDs)))
	if err := handler.SyncCommands(b.client, Commands, guildIDs); err != nil {
		slog.Warn("failed to sync slash commands", slog.Any("err", err))
	}

	if selfUser, ok := b.client.Caches.SelfUser(); ok {
		slog.Info("owner bot invite URL",
			slog.String("url", installOwnerURL(selfUser.ID)),
		)
	}

	slog.Info("bot is running. Press Ctrl+C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down...")

	return nil
}
