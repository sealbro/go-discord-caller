//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// Harness holds all bot connections shared across E2E tests.
// Create once in TestMain via newHarness; close via Shutdown after all tests run.
type Harness struct {
	cfg      *e2eConfig
	Owner    *bot.Client
	OwnerID  snowflake.ID
	Pool     *pool.Service
	Source   *SourceBot
	Source2  *SourceBot // nil when E2E_SOURCE_BOT_TOKEN_2 is unset
	Listener *ListenerBot
}

func newHarness(ctx context.Context, cfg *e2eConfig) (*Harness, error) {
	h := &Harness{cfg: cfg}

	// Owner bot — full intents + DAVE + FlagsAll cache, no slash-command router.
	var err error
	h.Owner, err = newOwnerClient(cfg.OwnerToken)
	if err != nil {
		return nil, fmt.Errorf("build owner client: %w", err)
	}
	if err := h.Owner.OpenGateway(ctx); err != nil {
		return nil, fmt.Errorf("open owner gateway: %w", err)
	}
	h.OwnerID, _ = guild.BotUserID(cfg.OwnerToken)

	// Speaker pool.
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("e2e"))
	if err != nil {
		return nil, fmt.Errorf("build metrics: %w", err)
	}
	h.Pool = pool.NewService(&metrics.Pool)
	poolCtx, poolCancel := context.WithTimeout(ctx, 30*time.Second)
	h.Pool.ConnectPool(poolCtx, cfg.SpeakerTokens)
	poolCancel()

	// Harness bots.
	h.Source, err = newSourceBot(ctx, cfg.SourceToken)
	if err != nil {
		return nil, fmt.Errorf("build source bot: %w", err)
	}
	if cfg.SourceToken2 != "" {
		h.Source2, err = newSourceBot(ctx, cfg.SourceToken2)
		if err != nil {
			return nil, fmt.Errorf("build source bot 2: %w", err)
		}
	}
	h.Listener, err = newListenerBot(ctx, cfg.ListenerToken)
	if err != nil {
		return nil, fmt.Errorf("build listener bot: %w", err)
	}

	return h, nil
}

// NewManager creates a fresh manager.Service with a clean in-memory store.
// speakerChannelIDs are assigned to pool speakers in order (first pool speaker
// gets speakerChannelIDs[0], etc.). The owner and caller role are pre-bound.
// Always call manager.StopVoiceRaid + manager.Shutdown in t.Cleanup.
func (h *Harness) NewManager(speakerChannelIDs ...snowflake.ID) (*manager.Service, *store.InMemoryStore) {
	return h.newManagerForGuild(h.cfg.GuildID, h.cfg.OwnerChannelID, speakerChannelIDs...)
}

func (h *Harness) newManagerForGuild(guildID, ownerChannelID snowflake.ID, speakerChannelIDs ...snowflake.ID) (*manager.Service, *store.InMemoryStore) {
	st := store.NewInMemoryStore()
	metrics, _ := telemetry.NewMetrics(noop.NewMeterProvider().Meter("e2e"))
	svc := manager.NewService(st, h.Pool, h.Owner, h.OwnerID, config.TestConfig{AllowBots: true}, metrics)

	st.BindChannel(guildID, h.OwnerID, ownerChannelID)
	st.BindRole(guildID, store.RoleTypeCaller, h.cfg.CallerRoleID)

	speakerIDs := h.Pool.GetIDs()
	for i, chID := range speakerChannelIDs {
		if i >= len(speakerIDs) {
			break
		}
		st.BindChannel(guildID, speakerIDs[i], chID)
	}

	svc.SeedExistingSpeakers([]snowflake.ID{guildID})
	return svc, st
}

// Shutdown closes all bot connections. Call once after all tests complete.
func (h *Harness) Shutdown(ctx context.Context) {
	h.Pool.Shutdown(ctx)
	if h.Source != nil {
		h.Source.Close(ctx)
	}
	if h.Source2 != nil {
		h.Source2.Close(ctx)
	}
	if h.Listener != nil {
		h.Listener.Close(ctx)
	}
	h.Owner.Close(ctx)
}

// newOwnerClient builds a disgo client matching production settings but without
// slash-command event listeners. Full intents + DAVE + FlagsAll cache.
func newOwnerClient(token string) (*bot.Client, error) {
	return disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMembers,
				gateway.IntentGuildVoiceStates,
			),
		),
		bot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagsAll),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		),
	)
}
