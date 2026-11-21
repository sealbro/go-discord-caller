package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
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
// The start waits a bounded time for it instead.
func TestStartVoiceRaid_SpeakerReconnectingAtStart(t *testing.T) {
	cases := []struct {
		name     string
		backIn   time.Duration // 0: never comes back
		wantJoin bool
	}{
		{"back before the wait runs out", speakerReconnectWait - 200*time.Millisecond, true},
		{"still down when the wait runs out", speakerReconnectWait + 200*time.Millisecond, false},
		{"never comes back", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const (
					recoveringID     = snowflake.ID(501)
					recoveringChanID = snowflake.ID(902)
				)
				f := withCallerRole(newCallerRoleFixture(t))
				f.st.BindChannel(testGuildID, recoveringID, recoveringChanID)
				f.caches.AddChannel(voiceChannel(t, testGuildID, recoveringChanID))
				f.m.statuses[testGuildID].Speakers[recoveringID] = &guild.Speaker{ID: recoveringID, Enabled: true}

				var recoveringJoins atomic.Int32
				recoveringVM := &fakeVoiceManager{
					conn:   &fakeVoiceConn{channelID: recoveringChanID},
					onOpen: func() { recoveringJoins.Add(1) },
				}
				var down atomic.Bool
				down.Store(true)
				healthy, _ := f.m.poolSvc.GetClientByID(roleTestSpeakerID)
				f.m.poolSvc = &reconnectingPool{
					clients: map[snowflake.ID]*bot.Client{
						roleTestSpeakerID: healthy,
						recoveringID:      {VoiceManager: recoveringVM, Caches: cache.New(), Rest: stubRest{}},
					},
					ids:  []snowflake.ID{roleTestSpeakerID, recoveringID},
					down: map[snowflake.ID]*atomic.Bool{recoveringID: &down},
				}
				if tc.backIn > 0 {
					time.AfterFunc(tc.backIn, func() { down.Store(false) })
				}

				_, cancel, err := startHostRaid(t, f)
				defer cancel()
				if err != nil {
					t.Fatalf("StartVoiceRaid: %v", err)
				}
				defer func() { _ = f.m.StopVoiceRaid(context.Background(), testGuildID) }()

				if joined := recoveringJoins.Load() == 1; joined != tc.wantJoin {
					t.Errorf("speaker back after %v joined = %v, want %v (the start waits up to %v)",
						tc.backIn, joined, tc.wantJoin, speakerReconnectWait)
				}
			})
		})
	}
}
