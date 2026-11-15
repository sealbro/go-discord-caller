package manager

import (
	"context"
	"log/slog"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/bot/handlers"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const otherGuildID = snowflake.ID(5002)

type botOnlineHarness struct {
	t      *testing.T
	svc    *Service
	client *bot.Client
	reader *sdkmetric.ManualReader
}

func newBotOnlineHarness(t *testing.T) *botOnlineHarness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("bot_online_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	client := &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll))}
	client.EventManager = bot.NewEventManager(client, bot.WithGatewayHandlers(handlers.GetGatewayHandlers()))
	client.MemberChunkingManager = bot.NewMemberChunkingManager(client, slog.Default(), bot.MemberChunkingFilterNone)
	svc := &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       store.NewInMemoryStore(),
		poolSvc:     emptyPool{},
		ownerClient: client,
		ownerBotID:  testBotID,
		metrics:     metrics,
	}
	if err := metrics.Bot.RegisterBotOnline(svc.observeBotOnline); err != nil {
		t.Fatalf("RegisterBotOnline: %v", err)
	}
	return &botOnlineHarness{t: t, svc: svc, client: client, reader: reader}
}

func (h *botOnlineHarness) joinGuild(id snowflake.ID) {
	h.svc.mu.Lock()
	h.svc.statuses[id] = guild.NewStatus(id, testBotID)
	h.svc.mu.Unlock()
	handlers.GetGatewayHandlers()[gateway.EventTypeGuildCreate].
		HandleGatewayEvent(h.client, 0, 0, gateway.EventGuildCreate{ID: id})
}

func (h *botOnlineHarness) leaveGuild(id snowflake.ID) {
	handlers.GetGatewayHandlers()[gateway.EventTypeGuildDelete].
		HandleGatewayEvent(h.client, 0, 0, gateway.EventGuildDelete{ID: id})
}

func (h *botOnlineHarness) onlineGuilds() map[snowflake.ID]bool {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatalf("Collect: %v", err)
	}
	out := map[snowflake.ID]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.bot.online" {
				continue
			}
			gauge, ok := md.Data.(metricdata.Gauge[int64])
			if !ok {
				h.t.Fatalf("gdc.bot.online: unexpected data type %T", md.Data)
			}
			for _, dp := range gauge.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("guild_id"))
				id, err := snowflake.Parse(v.AsString())
				if err != nil {
					h.t.Fatalf("guild_id %q: %v", v.AsString(), err)
				}
				out[id] = true
			}
		}
	}
	return out
}

// TestBotOnline_FollowsGuildMembership: the guild state is created when the bot
// joins a guild but nothing removed it when the bot left, so gdc.bot.online kept
// reporting the guild until the process restarted.
func TestBotOnline_FollowsGuildMembership(t *testing.T) {
	t.Parallel()
	h := newBotOnlineHarness(t)

	h.joinGuild(testGuildID)
	if got := h.onlineGuilds(); !got[testGuildID] {
		t.Fatalf("after joining: guild %s missing from gdc.bot.online, got %v", testGuildID, got)
	}

	h.joinGuild(otherGuildID)
	h.leaveGuild(testGuildID)

	got := h.onlineGuilds()
	if got[testGuildID] {
		t.Errorf("after leaving: guild %s still reported by gdc.bot.online", testGuildID)
	}
	if !got[otherGuildID] {
		t.Errorf("after leaving another guild: guild %s dropped from gdc.bot.online", otherGuildID)
	}
}

func TestForgetGuild_DropsGuildState(t *testing.T) {
	t.Parallel()
	h := newBotOnlineHarness(t)
	h.joinGuild(testGuildID)
	h.joinGuild(otherGuildID)

	h.svc.ForgetGuild(context.Background(), testGuildID)

	h.svc.mu.RLock()
	defer h.svc.mu.RUnlock()
	if _, kept := h.svc.statuses[testGuildID]; kept {
		t.Errorf("guild %s state kept after the bot left", testGuildID)
	}
	if _, kept := h.svc.statuses[otherGuildID]; !kept {
		t.Errorf("guild %s state dropped by another guild's leave", otherGuildID)
	}
}

// gdc.bot.online used to carry one series per bot per guild, labelled with the
// bot's user ID, although the only thing read from it was how many bots a guild
// has. It is now one series per guild whose value is that count.
func TestBotOnline_OneSeriesPerGuildCountingBots(t *testing.T) {
	t.Parallel()
	h := newBotOnlineHarness(t)
	h.svc.poolSvc = fixedPool{}

	h.joinGuild(testGuildID)
	h.svc.mu.Lock()
	h.svc.statuses[testGuildID].Speakers[701] = &guild.Speaker{ID: 701, Enabled: true}
	h.svc.statuses[testGuildID].Speakers[702] = &guild.Speaker{ID: 702, Enabled: true}
	h.svc.mu.Unlock()

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var points []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "gdc.bot.online" {
				points = append(points, md.Data.(metricdata.Gauge[int64]).DataPoints...)
			}
		}
	}

	if len(points) != 1 {
		t.Fatalf("want one gdc.bot.online series for the guild, got %d: %+v", len(points), points)
	}
	if _, named := points[0].Attributes.Value(attribute.Key("user_id")); named {
		t.Errorf("gdc.bot.online still carries a user_id label: %v", points[0].Attributes)
	}
	if want := int64(3); points[0].Value != want {
		t.Errorf("want %d bots (owner and two speakers), got %d", want, points[0].Value)
	}
}
