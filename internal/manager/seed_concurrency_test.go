package manager

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

type fixedPool struct{ emptyPool }

func (fixedPool) GetIDs() []snowflake.ID { return []snowflake.ID{701, 702, 703} }

// seedRest is a Discord where no pool bot is in any guild. Every member lookup
// first passes onLookup, which a test uses to hold lookups in flight.
type seedRest struct {
	rest.Rest
	requests atomic.Int32
	onLookup func()
}

func (r *seedRest) GetMember(snowflake.ID, snowflake.ID, ...rest.RequestOpt) (*discord.Member, error) {
	r.requests.Add(1)
	if r.onLookup != nil {
		r.onLookup()
	}
	return nil, errors.New("unknown member")
}

func (r *seedRest) GetGuildChannels(snowflake.ID, ...rest.RequestOpt) ([]discord.GuildChannel, error) {
	r.requests.Add(1)
	return nil, nil
}

func newSeedService(rst *seedRest) *Service {
	return &Service{
		statuses:    make(map[snowflake.ID]*guild.Status),
		store:       store.NewInMemoryStore(),
		poolSvc:     fixedPool{},
		ownerClient: &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll)), Rest: rst},
		ownerBotID:  testBotID,
		reconnect:   newReconnectState(),
	}
}

// After a restart the member cache is cold, so seeding a guild costs one REST
// round trip per pool bot (twice: the membership check and the cache warm-up).
// Seeding the guilds one after another makes the window in which /setup shows no
// speakers grow with the number of guilds; observed in production 2026-11-01,
// where two restarts left the guilds unseeded for 8 and 4 minutes.
//
// Guilds have independent REST rate-limit buckets, so seeding them must overlap:
// every guild's first lookup is held until all four are in flight.
func TestSeedExistingSpeakers_SeedsGuildsConcurrently(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		guilds := []snowflake.ID{11, 12, 13, 14}
		allInFlight := newMeetingPoint(len(guilds))
		rst := &seedRest{onLookup: func() { allInFlight.arrive(t) }}

		newSeedService(rst).SeedExistingSpeakers(guilds)
	})
}

// The setup UI calls EnsureGuildReady while the startup seeding may still be
// working through the same guild. Running a second seeding alongside it repeats
// every lookup against a rate-limit bucket the first one is already draining.
func TestEnsureGuildReady_JoinsSeedingAlreadyInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		baseline := &seedRest{}
		newSeedService(baseline).SeedExistingSpeakers([]snowflake.ID{11})
		perSeeding := baseline.requests.Load()

		hold := make(chan struct{})
		rst := &seedRest{onLookup: func() { <-hold }}
		m := newSeedService(rst)
		startup := make(chan struct{})
		go func() {
			m.SeedExistingSpeakers([]snowflake.ID{11})
			close(startup)
		}()
		synctest.Wait()

		ensured := make(chan struct{})
		go func() {
			m.EnsureGuildReady(11)
			close(ensured)
		}()
		synctest.Wait()
		close(hold)
		<-startup
		<-ensured

		if got := rst.requests.Load(); got != perSeeding {
			t.Errorf("REST requests = %d, want %d: EnsureGuildReady repeated the lookups of a seeding already in flight", got, perSeeding)
		}
	})
}
