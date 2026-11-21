package manager

import (
	"slices"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

func newBindingsService() *Service {
	return &Service{store: store.NewInMemoryStore(), ownerBotID: testBotID}
}

func TestOwnerBotID(t *testing.T) {
	if got := newBindingsService().OwnerBotID(); got != testBotID {
		t.Fatalf("OwnerBotID = %v, want %v", got, testBotID)
	}
}

func TestChannelBindingRoundTrip(t *testing.T) {
	m := bindingsService(store.NewInMemoryStore(), &bot.Client{Caches: cache.New()})
	const ch snowflake.ID = 77

	m.BindChannel(testGuildID, testBotID, ch)
	if got, ok := m.GetBoundChannel(testGuildID, testBotID); !ok || got != ch {
		t.Fatalf("GetBoundChannel = %v, %v; want %v, true", got, ok, ch)
	}

	m.UnbindChannel(testGuildID, testBotID)
	if _, ok := m.GetBoundChannel(testGuildID, testBotID); ok {
		t.Fatal("channel still bound after UnbindChannel")
	}
}

func TestLocaleBindingRoundTrip(t *testing.T) {
	m := newBindingsService()

	if got := m.GetLocale(testGuildID); got != "" {
		t.Fatalf("GetLocale before binding = %q, want empty", got)
	}
	m.BindLocale(testGuildID, "fr")
	if got := m.GetLocale(testGuildID); got != "fr" {
		t.Fatalf("GetLocale = %q, want fr", got)
	}
	m.UnbindLocale(testGuildID)
	if got := m.GetLocale(testGuildID); got != "" {
		t.Fatalf("GetLocale after UnbindLocale = %q, want empty", got)
	}
}

func TestHasManagerRole(t *testing.T) {
	m := newBindingsService()
	const (
		managerRole snowflake.ID = 100
		otherRole   snowflake.ID = 200
	)

	if m.HasManagerRole(testGuildID, []snowflake.ID{managerRole}) {
		t.Fatal("HasManagerRole true with no manager role bound")
	}

	m.BindRole(testGuildID, store.RoleTypeManager, managerRole)
	if !m.HasManagerRole(testGuildID, []snowflake.ID{otherRole, managerRole}) {
		t.Fatal("HasManagerRole false for a member holding the manager role")
	}
	if m.HasManagerRole(testGuildID, []snowflake.ID{otherRole}) {
		t.Fatal("HasManagerRole true for a member without the manager role")
	}

	m.UnbindRole(testGuildID, store.RoleTypeManager)
	if m.HasManagerRole(testGuildID, []snowflake.ID{managerRole}) {
		t.Fatal("HasManagerRole true after UnbindRole")
	}
}

func TestResetAllyCodeReplacesTheStoredCode(t *testing.T) {
	m := newBindingsService()
	old := m.store.GetOrCreateAllyCode(testGuildID)

	code := m.ResetAllyCode(testGuildID)

	if code == "" || code == old {
		t.Fatalf("ResetAllyCode = %q, want a fresh code replacing %q", code, old)
	}
	if stored, _ := m.store.GetAllyCode(testGuildID); stored != code {
		t.Errorf("stored code = %q, want the returned %q", stored, code)
	}
}

type debounceRecorder struct {
	mu       sync.Mutex
	channels []snowflake.ID
}

func (r *debounceRecorder) Debounce(channelID snowflake.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, channelID)
}

func TestAutoRouteReachesOnlyTheGuildsRouter(t *testing.T) {
	m := newBindingsService()
	router := &debounceRecorder{}
	routers := map[snowflake.ID]guild.AutoRouter{testGuildID: router}
	m.activeRouters.Store(&routers)

	m.AutoRoute(testGuildID, 77)
	m.AutoRoute(testGuildID+1, 78)

	if !slices.Equal(router.channels, []snowflake.ID{77}) {
		t.Errorf("debounced channels = %v, want only [77]", router.channels)
	}

	(&Service{}).AutoRoute(testGuildID, 77) // no routers at all: a no-op
}
