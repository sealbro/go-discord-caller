package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// emptyPool is a PoolService with no speaker bots, so seedGuildSpeakers reaches
// no Discord call and only creates the guild status.
type emptyPool struct{}

func (emptyPool) GetClientByID(snowflake.ID) (*bot.Client, bool) { return nil, false }
func (emptyPool) GetIDs() []snowflake.ID                         { return nil }
func (emptyPool) Reconnect(context.Context, snowflake.ID) bool   { return false }
func (emptyPool) Shutdown(context.Context)                       {}

// Startup seeding runs as a detached goroutine over every guild in the Ready
// payload (onReady → go SeedExistingSpeakers), taking seconds per guild, while
// slash commands are usable from the moment the process starts — the guild
// commands are already registered on Discord's side from the previous run, so
// nothing gates a /start on seeding having finished.
//
// A /start landing in that window hit ErrNoGuildStatus and told the operator to
// "seed the guild first", which is not an action they can take. Observed in
// production 2026-09-22T18:31:37Z: /start 6 s before that guild's seeding
// completed at 18:31:43Z.
//
// Starting a raid in a guild seeding has not reached yet must seed it on demand,
// so the operator sees the guild's real state — here "nothing bound" — instead.
func TestSetupSpeakers_SeedsGuildNotYetReachedByStartupSeeding(t *testing.T) {
	t.Parallel()

	m := &Service{
		statuses:   make(map[snowflake.ID]*guild.Status),
		store:      store.NewInMemoryStore(),
		poolSvc:    emptyPool{},
		ownerBotID: testBotID,
		reconnect:  newReconnectState(),
	}

	_, err := m.setupSpeakers(context.Background(), testGuildID, guild.RaidModeOneCaller, nil, telemetry.GuildMetrics{})

	if errors.Is(err, ErrNoGuildStatus) {
		t.Fatalf("unseeded guild must be seeded on demand, got %v", err)
	}
	if !errors.Is(err, ErrNoBoundSpeakers) {
		t.Fatalf("want ErrNoBoundSpeakers after on-demand seeding, got %v", err)
	}
	if _, ok := m.statuses[testGuildID]; !ok {
		t.Error("guild status must exist after on-demand seeding")
	}
}
