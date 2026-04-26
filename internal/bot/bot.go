package bot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel"
)

// SessionManager handles voice raid session lifecycle.
type SessionManager interface {
	StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (ally.Code, error)
	StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error
	JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode, code ally.Code) (guild.RaidMode, error)
	HasActiveSession(guildID snowflake.ID) bool
	UpdateMixerPause(guildID snowflake.ID)
	CheckGuildChannelAccess(guildID snowflake.ID) []manager.ChannelAccessWarning
	ReconnectBotChannel(ctx context.Context, guildID, botUserID snowflake.ID)
	OnBotVoiceMove(ctx context.Context, guildID, botUserID snowflake.ID, currentChannelID *snowflake.ID)
}

// BindingManager handles channel and role bindings.
type BindingManager interface {
	BindRole(guildID snowflake.ID, roleType store.RoleType, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID, roleType store.RoleType)
	BindChannel(guildID, userID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)
	OwnerBotID() snowflake.ID
}

// SpeakerManager handles speaker registration and configuration.
type SpeakerManager interface {
	ToggleSpeaker(guildID, speakerID snowflake.ID, enabled bool) error
	NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool)
	HasAvailableToken(guildID snowflake.ID) bool
}

// StatusProvider provides read-only status and authorization queries.
type StatusProvider interface {
	GetStatus(guildID snowflake.ID) guild.Status
	HasManagerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool
	HasCallerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool
}

// SeedManager handles speaker bot registration on startup and guild events.
type SeedManager interface {
	SeedExistingSpeakers(guildIDs []snowflake.ID)
	TrySeedMember(guildID, newUserID snowflake.ID)
	RemoveSpeaker(guildID, userID snowflake.ID)
}

// LifecycleManager handles startup hooks and graceful shutdown.
type LifecycleManager interface {
	StartMetrics()
	Shutdown(ctx context.Context)
}

// ManagerService is the full interface consumed by the bot layer.
// It composes focused sub-interfaces for better separation of concerns.
type ManagerService interface {
	SessionManager
	BindingManager
	SpeakerManager
	StatusProvider
	SeedManager
	LifecycleManager
}

// Bot wraps the disgo client and all application services.
type Bot struct {
	client        *bot.Client
	manager       ManagerService
	store         store.Store
	poolSvc       *pool.Service
	speakerTokens []string
	guildReadyCh  chan []snowflake.ID
}

// New creates and configures a new Bot instance. It performs no network I/O —
// all connections are established in Run.
func New(cfg *config.Config) (*Bot, error) {
	// Command router
	r := handler.New()

	// Buffered channel (cap 1) receives guild IDs from the Ready event for command sync.
	guildReadyCh := make(chan []snowflake.ID, 1)

	// Manager (owner) bot client
	client, err := newOwnerClient(cfg.OwnerBotToken, r)
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
	ownerBotID, ok := guild.BotUserID(cfg.OwnerBotToken)
	if !ok {
		return nil, fmt.Errorf("failed to get owner bot ID from token")
	}
	st, err := store.NewYAMLStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open yaml store %q: %w", cfg.StorePath, err)
	}

	// Metrics — must be created after telemetry.Setup so the OTel SDK is initialised.
	metrics, err := telemetry.NewMetrics(otel.Meter(telemetry.ServiceName))
	if err != nil {
		return nil, fmt.Errorf("failed to init metrics: %w", err)
	}

	poolSvc := pool.NewService(&metrics.Pool)
	managerSvc := manager.NewService(st, poolSvc, client, ownerBotID, cfg.Test, metrics)

	// Wire command handlers.
	cmdHandlers := NewCommandHandlers(managerSvc, &metrics.Bot)
	cmdHandlers.Register(r)

	client.AddEventListeners(eventListeners(managerSvc, &metrics.Bot)...)

	return &Bot{
		client:        client,
		manager:       managerSvc,
		store:         st,
		poolSvc:       poolSvc,
		speakerTokens: cfg.SpeakerTokens,
		guildReadyCh:  guildReadyCh,
	}, nil
}

// Run connects the speaker pool, opens the owner gateway, registers slash commands,
// and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := connectPool(ctx, b.poolSvc, b.speakerTokens); err != nil {
		return err
	}
	b.poolSvc.StartWatchdog(ctx, 30*time.Second)
	b.poolSvc.StartMetrics()
	b.manager.StartMetrics()

	if err := b.client.OpenGateway(ctx); err != nil {
		return err
	}
	defer func() {
		// Graceful shutdown: stop all raids, close all speaker gateways, then the owner gateway.
		shutdownCtx := context.Background()
		b.manager.Shutdown(shutdownCtx)
		b.store.Close()
		b.client.Close(shutdownCtx)
	}()

	// Wait for the Ready event to deliver guild IDs, then sync slash commands.
	// Falls back to global sync on timeout.
	var guildIDs []snowflake.ID
	select {
	case guildIDs = <-b.guildReadyCh:
	case <-time.After(10 * time.Second):
		slog.WarnContext(ctx, "timed out waiting for Ready event, syncing commands globally")
	}
	slog.InfoContext(ctx, "discovered guilds for command sync", slog.Int("count", len(guildIDs)))
	if err := handler.SyncCommands(b.client, Commands, guildIDs); err != nil {
		slog.WarnContext(ctx, "failed to sync slash commands", slog.Any("err", err))
	}

	if selfUser, ok := b.client.Caches.SelfUser(); ok {
		slog.InfoContext(ctx, "owner bot invite URL",
			slog.String("url", installOwnerURL(selfUser.ID)),
		)
		b.poolSvc.RegisterBot(selfUser.ID, selfUser.Username)
	}

	slog.InfoContext(ctx, "bot is running. Press Ctrl+C to stop.")

	<-ctx.Done()

	slog.Info("shutting down...")

	return nil
}

// newOwnerClient builds the disgo client for the owner (manager) bot.
func newOwnerClient(token string, r handler.Router) (*bot.Client, error) {
	return disgo.New(token,
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
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		),
	)
}

// connectPool opens one gateway per speaker token and fails if any are not connected.
func connectPool(ctx context.Context, poolSvc *pool.Service, tokens []string) error {
	poolCtx, poolCancel := context.WithTimeout(ctx, 30*time.Second)
	poolSvc.ConnectPool(poolCtx, tokens)
	poolCancel()

	total := len(poolSvc.GetIDs())
	connected := len(poolSvc.GetClients())
	if connected < total {
		return fmt.Errorf("speaker pool: only %d/%d speaker gateways connected at startup", connected, total)
	}
	slog.InfoContext(ctx, "speaker pool ready", slog.Int("total", total))
	return nil
}
