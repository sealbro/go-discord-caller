//go:build integration

package test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/disgoorg/disgo"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	internalbot "github.com/sealbro/go-discord-caller/internal/bot"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// Harness holds all bot connections shared across integration tests.
// Create once in TestMain via newHarness; close via Shutdown after all tests run.
type Harness struct {
	Cfg             *Config
	Owner           *disgobot.Client
	OwnerID         snowflake.ID
	Pool            *pool.Service
	Speaker         *Speaker
	Speaker2        *Speaker // nil when E2E_SOURCE_BOT_TOKEN_2 is unset (required for E2/E6)
	Listener        *Listener
	activeListeners []disgobot.EventListener // current per-test listeners; swapped in newManagerForGuild
}

func NewHarness(ctx context.Context, cfg *Config) (*Harness, error) {
	h := &Harness{Cfg: cfg}

	// Owner bot — full intents + DAVE + FlagsAll cache, no slash-command router.
	var err error
	h.Owner, err = newOwnerClient(cfg.OwnerBotToken)
	if err != nil {
		return nil, fmt.Errorf("build owner client: %w", err)
	}
	if err := h.Owner.OpenGateway(ctx); err != nil {
		return nil, fmt.Errorf("open owner gateway: %w", err)
	}
	h.OwnerID, _ = guild.BotUserID(cfg.OwnerBotToken)

	// Speaker pool.
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("integration"))
	if err != nil {
		return nil, fmt.Errorf("build metrics: %w", err)
	}
	h.Pool = pool.NewService(&metrics.Pool)
	poolCtx, poolCancel := context.WithTimeout(ctx, 30*time.Second)
	h.Pool.ConnectPool(poolCtx, cfg.SpeakerTokens)
	poolCancel()

	// Harness bots.
	h.Speaker, err = newTestSpeaker(ctx, cfg.SourceToken)
	if err != nil {
		return nil, fmt.Errorf("build test speaker: %w", err)
	}
	if cfg.SourceToken2 != "" {
		h.Speaker2, err = newTestSpeaker(ctx, cfg.SourceToken2)
		if err != nil {
			return nil, fmt.Errorf("build test speaker 2: %w", err)
		}
	}
	h.Listener, err = newTestListener(ctx, cfg.ListenerToken)
	if err != nil {
		return nil, fmt.Errorf("build test listener: %w", err)
	}

	return h, nil
}

// NewManager creates a fresh manager.Service with a clean in-memory store.
// speakerChannelIDs are assigned to pool speakers in order (first pool speaker
// gets speakerChannelIDs[0], etc.). The owner and caller role are pre-bound.
// Always call manager.StopVoiceRaid in t.Cleanup.
func (h *Harness) NewManager(speakerChannelIDs ...snowflake.ID) (*manager.Service, *store.InMemoryStore) {
	return h.newManagerForGuild(h.Cfg.GuildID, h.Cfg.OwnerChannelID, speakerChannelIDs...)
}

func (h *Harness) newManagerForGuild(guildID, ownerChannelID snowflake.ID, speakerChannelIDs ...snowflake.ID) (*manager.Service, *store.InMemoryStore) {
	st := store.NewInMemoryStore()
	metrics, _ := telemetry.NewMetrics(noop.NewMeterProvider().Meter("integration"))
	svc := manager.NewService(st, h.Pool, h.Owner, h.OwnerID, config.TestConfig{AllowBots: true}, metrics)

	st.BindChannel(guildID, h.OwnerID, ownerChannelID)
	st.BindRole(guildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	for i, chID := range speakerChannelIDs {
		if i >= len(speakerIDs) {
			break
		}
		st.BindChannel(guildID, speakerIDs[i], chID)
	}

	svc.SeedExistingSpeakers([]snowflake.ID{guildID})

	// Wire the same event handlers that production uses so onVoiceLeave and
	// onVoiceMove fire and drive ReconnectBotChannel during tests. Remove the
	// previous test's listeners first so they don't accumulate across tests.
	if len(h.activeListeners) > 0 {
		h.Owner.RemoveEventListeners(h.activeListeners...)
	}
	h.activeListeners = internalbot.EventListeners(svc, &metrics.Bot)
	h.Owner.AddEventListeners(h.activeListeners...)

	return svc, st
}

// DisconnectSpeakerVoice drops the speaker bot's voice connection in the guild
// by sending OP4 with channel_id=null, triggering VOICE_STATE_UPDATE on Discord
// so the owner bot's onVoiceLeave handler fires and calls ReconnectBotChannel.
func (h *Harness) DisconnectSpeakerVoice(ctx context.Context, guildID, speakerID snowflake.ID) {
	client, ok := h.Pool.GetClientByID(speakerID)
	if !ok {
		return
	}
	pool.NewGuildVoice(client.VoiceManager, 0).Leave(ctx, guildID)
}

// MoveSpeakerVoice simulates an admin dragging a speaker bot into targetChannelID.
// Uses the listener bot's REST client (the designated test-admin bot with
// Administrator in the test guild) so the owner bot can keep production-identical
// permissions. Triggers GuildVoiceMove on the owner bot → onVoiceMove →
// OnBotVoiceMove → ReconnectBotChannel.
func (h *Harness) MoveSpeakerVoice(_ context.Context, guildID, speakerID, targetChannelID snowflake.ID) error {
	_, err := h.Listener.UpdateMember(guildID, speakerID, discord.MemberUpdate{
		ChannelID: &targetChannelID,
	})
	return err
}

// RequireSpeakers returns connected speaker IDs, skipping the test if the pool is empty.
func (h *Harness) RequireSpeakers(t testing.TB) []snowflake.ID {
	t.Helper()
	ids := h.Pool.ConnectedSpeakerIDs()
	if len(ids) == 0 {
		t.Skip("no speakers in pool")
	}
	return ids
}

// MustStartRaid creates a manager, starts a voice raid for h.Cfg.GuildID, and fatals on error.
// The relay code is logged via t.Log. Pass a dedicated sessionCancel when the session
// lifecycle must be independent of the test context (e.g. restart or reconnect tests).
func (h *Harness) MustStartRaid(t testing.TB, ctx context.Context, cancel context.CancelFunc, mode guild.RaidMode, speakerChannelIDs ...snowflake.ID) *manager.Service {
	t.Helper()
	mgr, _ := h.NewManager(speakerChannelIDs...)
	code, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, mode)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	t.Logf("relay code: %s", code)
	return mgr
}

// MustStartPlaying calls speaker.StartPlaying and fatals on error.
func (h *Harness) MustStartPlaying(t testing.TB, ctx context.Context, speaker *Speaker, channelID snowflake.ID) func() {
	t.Helper()
	stop, err := speaker.StartPlaying(ctx, h.Cfg.GuildID, channelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlaying: %v", err)
	}
	return stop
}

// MustStartListening calls Listener.StartListening and fatals on error.
func (h *Harness) MustStartListening(t testing.TB, ctx context.Context, guildID, channelID snowflake.ID) func() {
	t.Helper()
	stop, err := h.Listener.StartListening(ctx, guildID, channelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}
	return stop
}

// RegisterCleanup registers t.Cleanup to call each stop func then StopVoiceRaid for h.Cfg.GuildID.
// Do not use when stop funcs are reassigned after registration (e.g. E11) or when multiple
// guild IDs need separate StopVoiceRaid calls (e.g. E5).
func (h *Harness) RegisterCleanup(t testing.TB, mgr *manager.Service, stops ...func()) {
	t.Helper()
	t.Cleanup(func() {
		for _, stop := range stops {
			stop()
		}
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})
}

// Shutdown closes all bot connections. Call once after all tests complete.
func (h *Harness) Shutdown(ctx context.Context) {
	h.Pool.Shutdown(ctx)
	if h.Speaker != nil {
		h.Speaker.Close(ctx)
	}
	if h.Speaker2 != nil {
		h.Speaker2.Close(ctx)
	}
	if h.Listener != nil {
		h.Listener.Close(ctx)
	}
	h.Owner.Close(ctx)
}

// newOwnerClient builds a disgo client matching production settings but without
// slash-command event listeners. Full intents + DAVE + FlagsAll cache.
// Event listeners are registered per-test via newManagerForGuild.
func newOwnerClient(token string) (*disgobot.Client, error) {
	return disgo.New(token,
		disgobot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMembers,
				gateway.IntentGuildVoiceStates,
			),
		),
		disgobot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagsAll),
		),
		disgobot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		),
	)
}
