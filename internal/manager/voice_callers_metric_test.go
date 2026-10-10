package manager

import (
	"context"
	"log/slog"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/bot/handlers"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	callerRoleID  = snowflake.ID(77)
	callerChannel = snowflake.ID(1001)
	otherChannel  = snowflake.ID(2002)
)

// callersHarness feeds gateway events through disgo's own handlers into the
// owner client's cache, as production does, and collects gdc.voice.callers.
type callersHarness struct {
	t       *testing.T
	client  *bot.Client
	reader  *sdkmetric.ManualReader
	members map[snowflake.ID]discord.Member
}

func newCallersHarness(t *testing.T) *callersHarness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("voice_callers_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	client := &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll))}
	client.EventManager = bot.NewEventManager(client, bot.WithGatewayHandlers(handlers.GetGatewayHandlers()))
	client.MemberChunkingManager = bot.NewMemberChunkingManager(client, slog.Default(), bot.MemberChunkingFilterNone)

	st := store.NewInMemoryStore()
	st.BindRole(testGuildID, store.RoleTypeCaller, callerRoleID)

	m := &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       st,
		ownerClient: client,
		ownerBotID:  testBotID,
		metrics:     metrics,
	}
	if err := metrics.Bot.RegisterVoiceCallers(m.observeVoiceCallers); err != nil {
		t.Fatalf("RegisterVoiceCallers: %v", err)
	}
	return &callersHarness{t: t, client: client, reader: reader, members: map[snowflake.ID]discord.Member{}}
}

func (h *callersHarness) dispatch(eventType gateway.EventType, event gateway.EventData) {
	handlers.GetGatewayHandlers()[eventType].HandleGatewayEvent(h.client, 0, 0, event)
}

func (h *callersHarness) member(userID snowflake.ID, bot bool, roles ...snowflake.ID) discord.Member {
	m := discord.Member{GuildID: testGuildID, User: discord.User{ID: userID, Bot: bot}, RoleIDs: roles}
	h.members[userID] = m
	return m
}

// startup replays what the gateway sends on boot: READY marks the guild
// unready, then GUILD_CREATE carries the members and voice states.
func (h *callersHarness) startup(members []discord.Member, voiceStates []discord.VoiceState) {
	h.client.Caches.SetGuildUnready(testGuildID, true)
	h.dispatch(gateway.EventTypeGuildCreate, gateway.EventGuildCreate{GatewayGuild: discord.GatewayGuild{
		RestGuild:   discord.RestGuild{Guild: discord.Guild{ID: testGuildID}},
		Members:     members,
		VoiceStates: voiceStates,
	}})
}

func (h *callersHarness) voiceState(userID snowflake.ID, channelID *snowflake.ID) {
	h.dispatch(gateway.EventTypeVoiceStateUpdate, gateway.EventVoiceStateUpdate{
		VoiceState: discord.VoiceState{GuildID: testGuildID, UserID: userID, ChannelID: channelID},
		Member:     h.members[userID],
	})
}

func (h *callersHarness) setRoles(userID snowflake.ID, roles ...snowflake.ID) {
	m := h.members[userID]
	m.RoleIDs = roles
	h.members[userID] = m
	h.dispatch(gateway.EventTypeGuildMemberUpdate, gateway.EventGuildMemberUpdate{Member: m})
}

func (h *callersHarness) collect() map[snowflake.ID]int64 {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatalf("Collect: %v", err)
	}
	out := map[snowflake.ID]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.voice.callers" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				h.t.Fatalf("gdc.voice.callers: unexpected data type %T", md.Data)
			}
			for _, dp := range sum.DataPoints {
				ch, _ := dp.Attributes.Value(attribute.Key("channel_id"))
				id, err := snowflake.Parse(ch.AsString())
				if err != nil {
					h.t.Fatalf("channel_id %q: %v", ch.AsString(), err)
				}
				out[id] += dp.Value
			}
		}
	}
	return out
}

func (h *callersHarness) expect(step string, want map[snowflake.ID]int64) {
	h.t.Helper()
	got := h.collect()
	for _, ch := range []snowflake.ID{callerChannel, otherChannel} {
		if got[ch] != want[ch] {
			h.t.Errorf("%s: channel %s want %d callers, got %d", step, ch, want[ch], got[ch])
		}
	}
}

func ptr(id snowflake.ID) *snowflake.ID { return &id }

// TestVoiceCallers_CallerPresentAtStartup is the production bug: on boot disgo
// dispatches GuildReady, not GuildAvailable, so a running count seeded only on
// GuildAvailable missed every caller already in voice and went negative when
// they left.
func TestVoiceCallers_CallerPresentAtStartup(t *testing.T) {
	t.Parallel()
	h := newCallersHarness(t)
	caller := h.member(123, false, callerRoleID)

	h.startup([]discord.Member{caller}, []discord.VoiceState{
		{GuildID: testGuildID, UserID: 123, ChannelID: ptr(callerChannel)},
	})
	h.expect("after startup", map[snowflake.ID]int64{callerChannel: 1})

	h.voiceState(123, nil)
	h.expect("after the caller leaves", nil)
}

// TestVoiceCallers_RoleChangeMidCall: a running count checked the role only on
// join and leave, so a role granted or revoked mid-call left it off by one.
func TestVoiceCallers_RoleChangeMidCall(t *testing.T) {
	t.Parallel()
	h := newCallersHarness(t)
	h.startup(nil, nil)
	h.member(123, false)

	h.voiceState(123, ptr(callerChannel))
	h.expect("joined without the role", nil)

	h.setRoles(123, callerRoleID)
	h.expect("role granted mid-call", map[snowflake.ID]int64{callerChannel: 1})

	h.setRoles(123)
	h.expect("role revoked mid-call", nil)

	h.voiceState(123, nil)
	h.expect("after leaving", nil)
}

func TestVoiceCallers_FollowsMoveAndSkipsBotsAndNonCallers(t *testing.T) {
	t.Parallel()
	h := newCallersHarness(t)
	h.startup(nil, nil)
	h.member(1, false, callerRoleID)
	h.member(2, false, callerRoleID)
	h.member(3, false)
	h.member(4, true, callerRoleID)

	for _, id := range []snowflake.ID{1, 2, 3, 4} {
		h.voiceState(id, ptr(callerChannel))
	}
	h.expect("all joined", map[snowflake.ID]int64{callerChannel: 2})

	h.voiceState(2, ptr(otherChannel))
	h.expect("one caller moved", map[snowflake.ID]int64{callerChannel: 1, otherChannel: 1})

	for _, id := range []snowflake.ID{1, 2, 3, 4} {
		h.voiceState(id, nil)
	}
	h.expect("all left", nil)
}
