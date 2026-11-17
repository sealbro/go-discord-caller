package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// reconnectingPool is a PoolService that refuses a speaker while its flag in
// down is set, the way the real pool refuses a bot whose gateway disgo is
// still resuming.
type reconnectingPool struct {
	clients map[snowflake.ID]*bot.Client
	ids     []snowflake.ID
	down    map[snowflake.ID]*atomic.Bool
}

func (p *reconnectingPool) GetClientByID(id snowflake.ID) (*bot.Client, bool) {
	if down, ok := p.down[id]; ok && down.Load() {
		return nil, false
	}
	c, ok := p.clients[id]
	return c, ok
}
func (p *reconnectingPool) GetIDs() []snowflake.ID                     { return p.ids }
func (*reconnectingPool) Reconnect(context.Context, snowflake.ID) bool { return false }
func (*reconnectingPool) Shutdown(context.Context)                     {}

// A speaker whose gateway is mid-reconnect when /start runs is skipped, and
// nothing brings it back: the raid runs for its whole life without that bot.
func TestStartVoiceRaid_SpeakerReconnectingAtStartStillJoins(t *testing.T) {
	const (
		ownerChannelID   = snowflake.ID(900)
		healthyID        = snowflake.ID(500)
		healthyChannelID = snowflake.ID(901)
		recoveringID     = snowflake.ID(501)
		recoveringChanID = snowflake.ID(902)
		reconnectAfter   = 300 * time.Millisecond
		joinDeadline     = 5 * time.Second
	)

	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, testBotID, ownerChannelID)
	st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	st.BindChannel(testGuildID, healthyID, healthyChannelID)
	st.BindChannel(testGuildID, recoveringID, recoveringChanID)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("speaker_reconnecting_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	var recoveringJoins atomic.Int32
	healthyVM := &fakeVoiceManager{conn: &fakeVoiceConn{channelID: healthyChannelID}}
	recoveringVM := &fakeVoiceManager{
		conn:   &fakeVoiceConn{channelID: recoveringChanID},
		onOpen: func() { recoveringJoins.Add(1) },
	}
	ownerVM := &fakeVoiceManager{conn: &fakeVoiceConn{channelID: ownerChannelID}}

	var recoveringDown atomic.Bool
	recoveringDown.Store(true)
	pool := &reconnectingPool{
		clients: map[snowflake.ID]*bot.Client{
			healthyID:    {VoiceManager: healthyVM, Caches: cache.New(), Rest: stubRest{}},
			recoveringID: {VoiceManager: recoveringVM, Caches: cache.New(), Rest: stubRest{}},
		},
		ids:  []snowflake.ID{healthyID, recoveringID},
		down: map[snowflake.ID]*atomic.Bool{recoveringID: &recoveringDown},
	}

	m := &Service{
		statuses: map[snowflake.ID]*guild.Status{
			testGuildID: {
				GuildID:     testGuildID,
				OwnerUserID: testBotID,
				Speakers: map[snowflake.ID]*guild.Speaker{
					healthyID:    {ID: healthyID, Enabled: true},
					recoveringID: {ID: recoveringID, Enabled: true},
				},
			},
		},
		store:       st,
		poolSvc:     pool,
		ownerClient: &bot.Client{VoiceManager: ownerVM, Caches: cachesWithChannels(t, testGuildID, ownerChannelID, healthyChannelID, recoveringChanID), Rest: stubRest{}},
		ownerBotID:  testBotID,
		sessions:    ally.NewManager(),
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}
	empty := map[snowflake.ID]guild.AutoRouter{}
	m.activeRouters.Store(&empty)

	time.AfterFunc(reconnectAfter, func() { recoveringDown.Store(false) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller); err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	deadline := time.Now().Add(joinDeadline)
	for recoveringJoins.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if recoveringJoins.Load() == 0 {
		t.Errorf("speaker never joined: its gateway was back %v after the raid started, but the raid had already skipped it", reconnectAfter)
	}
}
