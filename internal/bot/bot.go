package bot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric"
)

// SessionManager handles voice raid session lifecycle.
type SessionManager interface {
	StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (ally.Code, error)
	StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error
	JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode, code ally.Code) (guild.RaidMode, error)
	HasActiveSession(guildID snowflake.ID) bool
	AutoRoute(guildID, channelID snowflake.ID)
	CheckGuildChannelAccess(guildID snowflake.ID) []manager.ChannelAccessWarning
	ReconnectBotChannel(ctx context.Context, guildID, botUserID snowflake.ID)
	OnBotVoiceMove(ctx context.Context, guildID, botUserID snowflake.ID, currentChannelID *snowflake.ID)
	NotifyMemberUpdate(guildID snowflake.ID, member discord.Member)
}

// BindingManager handles channel and role bindings.
type BindingManager interface {
	BindRole(guildID snowflake.ID, roleType store.RoleType, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID, roleType store.RoleType)
	BindChannel(guildID, userID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)
	OwnerBotID() snowflake.ID
	BindLocale(guildID snowflake.ID, locale string)
	UnbindLocale(guildID snowflake.ID)
	GetLocale(guildID snowflake.ID) string
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
	IsBot(user discord.User) bool
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
	bundle        *i18n.Bundle
}

// New creates and configures a new Bot instance. It performs no network I/O —
// all connections are established in Run.
// st is the persistent store; callers are responsible for creating it
// (e.g. store.NewYAMLStore for production, store.NewInMemoryStore for tests).
func New(cfg *config.Config, st store.Store, meter metric.Meter) (*Bot, error) {
	// Command router
	r := handler.New()

	// Buffered channel (cap 1) receives guild IDs from the Ready event for command sync.
	guildReadyCh := make(chan []snowflake.ID, 1)

	// Manager (owner) bot client — production adds GuildMessages intent and the command router.
	client, err := NewOwnerClient(cfg.OwnerBotToken,
		bot.WithGatewayConfigOpts(gateway.WithIntents(
			gateway.IntentGuilds,
			gateway.IntentGuildMembers,
			gateway.IntentGuildVoiceStates,
			gateway.IntentGuildMessages,
		)),
		bot.WithEventListeners(r),
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

	ownerBotID, ok := guild.BotUserID(cfg.OwnerBotToken)
	if !ok {
		return nil, fmt.Errorf("failed to get owner bot ID from token")
	}

	// Metrics — must be created after telemetry.Setup so the OTel SDK is initialised.
	metrics, err := telemetry.NewMetrics(meter)
	if err != nil {
		return nil, fmt.Errorf("failed to init metrics: %w", err)
	}

	poolSvc := pool.NewService(&metrics.Pool)
	managerSvc := manager.NewService(st, poolSvc, client, ownerBotID, cfg.Test, metrics)
	managerSvc.SetSessionIdleTimeout(cfg.SessionIdleTimeout)

	bundle, err := i18n.NewBundle()
	if err != nil {
		return nil, fmt.Errorf("failed to load i18n bundle: %w", err)
	}
	slog.Info("i18n bundle loaded", slog.Int("locales", len(bundle.Tags())))

	// Wire command handlers.
	cmdHandlers := NewCommandHandlers(managerSvc, &metrics.Bot, bundle)
	cmdHandlers.Register(r)

	b := &Bot{
		client:        client,
		manager:       managerSvc,
		store:         st,
		poolSvc:       poolSvc,
		speakerTokens: cfg.SpeakerTokens,
		guildReadyCh:  guildReadyCh,
		bundle:        bundle,
	}

	// Built before the listeners are attached so onGuildJoin can call back into
	// b.syncGuildCommands. No network I/O happens here — the gateway opens in Run.
	client.AddEventListeners(EventListeners(managerSvc, &metrics.Bot, b.syncGuildCommands)...)

	return b, nil
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
	var guildIDs []snowflake.ID
	select {
	case guildIDs = <-b.guildReadyCh:
	case <-time.After(10 * time.Second):
		slog.WarnContext(ctx, "timed out waiting for Ready event, skipping command sync")
	}
	b.syncCommands(ctx, guildIDs)

	if selfUser, ok := b.client.Caches.SelfUser(); ok {
		slog.InfoContext(ctx, "owner bot invite URL",
			slog.String("url", installOwnerURL(selfUser.ID)),
		)
		b.poolSvc.RegisterBot(selfUser.ID, selfUser.Username, b.client)
	}

	slog.InfoContext(ctx, "bot is running. Press Ctrl+C to stop.")

	<-ctx.Done()

	slog.Info("shutting down...")

	return nil
}

// syncCommands refreshes the owner bot's slash commands: any command edited in
// BuildCommands is overwritten and any removed one is deleted, because each
// per-guild sync fully replaces that guild's command set.
//
// Commands are registered per guild (instant propagation, unlike the ~1h global
// cache). Because we never register globally, the global set must stay empty:
// a global command left over from an earlier build shows up as a ghost duplicate
// that survives guild re-syncs and only clears with a manual delete + restart.
// Overwriting the global set with an empty list on every boot prevents that.
func (b *Bot) syncCommands(ctx context.Context, guildIDs []snowflake.ID) {
	commands := BuildCommands(b.bundle)

	appID := b.client.ApplicationID

	if _, err := b.client.Rest.SetGlobalCommands(appID, nil); err != nil {
		slog.WarnContext(ctx, "failed to clear global commands", slog.Any("err", err))
	}

	if len(guildIDs) == 0 {
		slog.WarnContext(ctx, "no guilds discovered, skipping guild command sync")
		return
	}

	slog.InfoContext(ctx, "syncing slash commands", slog.Int("guilds", len(guildIDs)))
	for _, gid := range guildIDs {
		b.setGuildCommands(ctx, gid, commands)
	}
}

// syncGuildCommands registers the full command set for a single guild.
//
// syncCommands only covers the guilds present in the Ready payload at boot, and
// because nothing is ever registered globally there is no fallback: a guild the
// bot joins while running would otherwise show no commands at all until the next
// restart. Wired into onGuildJoin so a fresh invite is usable immediately.
func (b *Bot) syncGuildCommands(ctx context.Context, guildID snowflake.ID) {
	b.setGuildCommands(ctx, guildID, BuildCommands(b.bundle))
}

// setGuildCommands replaces one guild's command set. Guild-scoped registration
// propagates instantly, unlike the ~1h global command cache.
func (b *Bot) setGuildCommands(ctx context.Context, guildID snowflake.ID, commands []discord.ApplicationCommandCreate) {
	if _, err := b.client.Rest.SetGuildCommands(b.client.ApplicationID, guildID, commands); err != nil {
		slog.ErrorContext(ctx, "failed to sync guild commands",
			slog.String("guild_id", guildID.String()),
			slog.Any("err", err),
		)
		return
	}

	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.CommandName())
	}
	slices.Sort(names)

	slog.InfoContext(ctx, "synced guild commands",
		slog.String("guild_id", guildID.String()),
		slog.Int("count", len(commands)),
		slog.String("commands", strings.Join(names, ", ")),
	)
}

// NewOwnerClient builds a disgo client for the owner (manager) bot.
// Base config covers DAVE E2EE voice and FlagsAll cache. Callers supply
// their own intents and any extra options (e.g. event listeners, extra intents).
func NewOwnerClient(token string, opts ...bot.ConfigOpt) (*bot.Client, error) {
	botUserID, _ := guild.BotUserID(token)

	base := []bot.ConfigOpt{
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagsAll)),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			pool.SafeUDPConnOpt(),
			voice.WithLogger(telemetry.VoiceLogger(botUserID)),
		),
	}
	return disgo.New(token, append(base, opts...)...)
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
