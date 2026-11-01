package manager

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// After a restart the member cache is cold, so seeding a guild costs one REST
// round trip per pool bot (twice: the membership check and the cache warm-up).
// Seeding the guilds one after another makes the window in which /setup shows no
// speakers grow with the number of guilds; observed in production 2026-11-01,
// where two restarts left the guilds unseeded for 8 and 4 minutes.
//
// Guilds have independent REST rate-limit buckets, so seeding them must overlap.
func TestSeedExistingSpeakers_SeedsGuildsConcurrently(t *testing.T) {
	t.Parallel()

	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
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

	m := &Service{
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

	guilds := []snowflake.ID{11, 12, 13, 14}
	done := make(chan struct{})
	go func() {
		m.SeedExistingSpeakers(guilds)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("seeding did not finish")
	}

	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrent REST requests = %d across %d guilds, want guilds seeded concurrently", got, len(guilds))
	}
}
