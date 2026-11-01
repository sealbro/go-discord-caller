package manager

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

type fixedPool struct{ emptyPool }

func (fixedPool) GetIDs() []snowflake.ID { return []snowflake.ID{701, 702, 703} }

// seedHarness is a Service whose owner client talks to a fake Discord that
// answers after a delay, with every pool bot reported as not in the guild.
type seedHarness struct {
	svc      *Service
	requests atomic.Int32
	peak     atomic.Int32
	firstHit chan struct{}
}

func newSeedHarness(t *testing.T) *seedHarness {
	t.Helper()
	h := &seedHarness{firstHit: make(chan struct{})}
	var once sync.Once
	var inFlight atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(h.firstHit) })
		h.requests.Add(1)
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := h.peak.Load()
			if n <= p || h.peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/channels") {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"code":10007,"message":"Unknown Member"}`)
	}))
	t.Cleanup(srv.Close)

	h.svc = &Service{
		statuses: make(map[snowflake.ID]*guild.Status),
		store:    store.NewInMemoryStore(),
		poolSvc:  fixedPool{},
		ownerClient: &bot.Client{
			Caches: cache.New(cache.WithCaches(cache.FlagsAll)),
			Logger: slog.New(slog.DiscardHandler),
			Rest:   rest.New(rest.NewClient("", rest.WithURL(srv.URL))),
		},
		ownerBotID: testBotID,
		reconnect:  newReconnectState(),
	}
	return h
}

func waitSeeded(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("seeding did not finish")
	}
}

// After a restart the member cache is cold, so seeding a guild costs one REST
// round trip per pool bot (twice: the membership check and the cache warm-up).
// Seeding the guilds one after another makes the window in which /setup shows no
// speakers grow with the number of guilds; observed in production 2026-11-01,
// where two restarts left the guilds unseeded for 8 and 4 minutes.
//
// Guilds have independent REST rate-limit buckets, so seeding them must overlap.
func TestSeedExistingSpeakers_SeedsGuildsConcurrently(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)

	guilds := []snowflake.ID{11, 12, 13, 14}
	done := make(chan struct{})
	go func() {
		h.svc.SeedExistingSpeakers(guilds)
		close(done)
	}()
	waitSeeded(t, done)

	if got := h.peak.Load(); got < 2 {
		t.Errorf("peak concurrent REST requests = %d across %d guilds, want guilds seeded concurrently", got, len(guilds))
	}
}

// The setup UI calls EnsureGuildReady while the startup seeding may still be
// working through the same guild. Running a second seeding alongside it repeats
// every lookup against a rate-limit bucket the first one is already draining.
func TestEnsureGuildReady_JoinsSeedingAlreadyInFlight(t *testing.T) {
	t.Parallel()

	baseline := newSeedHarness(t)
	baseline.svc.SeedExistingSpeakers([]snowflake.ID{11})
	perSeeding := baseline.requests.Load()

	h := newSeedHarness(t)
	startup := make(chan struct{})
	go func() {
		h.svc.SeedExistingSpeakers([]snowflake.ID{11})
		close(startup)
	}()
	<-h.firstHit
	h.svc.EnsureGuildReady(11)
	waitSeeded(t, startup)

	if got := h.requests.Load(); got != perSeeding {
		t.Errorf("REST requests = %d, want %d: EnsureGuildReady repeated the lookups of a seeding already in flight", got, perSeeding)
	}
}
