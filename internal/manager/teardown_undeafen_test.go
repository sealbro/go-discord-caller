package manager

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// memberUpdateRecorder accepts every member update and records the deaf
// changes, per member.
type memberUpdateRecorder struct {
	rest.Rest
	mu        sync.Mutex
	undeafens map[snowflake.ID]int
}

func (r *memberUpdateRecorder) UpdateMember(_ snowflake.ID, userID snowflake.ID, u discord.MemberUpdate, _ ...rest.RequestOpt) (*discord.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u.Deaf != nil && !*u.Deaf {
		r.undeafens[userID]++
	}
	return &discord.Member{}, nil
}

// A speaker deafened at join is undeafened once at teardown, before it leaves:
// Discord rejects the update once the member is out of voice. A second request
// would only spend the guild's member-update rate limit.
func TestTeardownUndeafensASpeakerOnce(t *testing.T) {
	const (
		speakerID = snowflake.ID(520)
		channelID = snowflake.ID(521)
	)
	st := store.NewInMemoryStore()
	st.BindChannel(testGuildID, speakerID, channelID)
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("teardown_undeafen_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	rec := &memberUpdateRecorder{undeafens: map[snowflake.ID]int{}}
	conn := &fakeVoiceConn{channelID: channelID}
	undeafenedBeforeLeave := false
	conn.setOnClose(func() {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		undeafenedBeforeLeave = rec.undeafens[speakerID] > 0
	})
	m := &Service{
		statuses: map[snowflake.ID]*guild.Status{
			testGuildID: {
				GuildID:     testGuildID,
				OwnerUserID: testBotID,
				Speakers:    map[snowflake.ID]*guild.Speaker{speakerID: {ID: speakerID, Enabled: true}},
			},
		},
		store: st,
		poolSvc: onePool{id: speakerID, client: &bot.Client{
			VoiceManager: &fakeVoiceManager{conn: conn},
			Caches:       cache.New(),
			Rest:         stubRest{},
		}},
		ownerClient: &bot.Client{Caches: cachesWithChannels(t, testGuildID, channelID), Rest: rec},
		ownerBotID:  testBotID,
		metrics:     metrics,
		reconnect:   newReconnectState(),
	}

	ctx := context.Background()
	setup, err := m.setupSpeakers(ctx, testGuildID, guild.RaidModeOneCaller, nil, metrics.ForGuild(ctx, testGuildID), time.Now())
	if err != nil {
		t.Fatalf("setupSpeakers: %v", err)
	}
	setup.SpeakerCleanup()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := rec.undeafens[speakerID]; got != 1 {
		t.Errorf("speaker undeafened %d times at teardown, want 1", got)
	}
	if !undeafenedBeforeLeave {
		t.Error("speaker left voice before it was undeafened; the flag would stay set on the member")
	}
}
