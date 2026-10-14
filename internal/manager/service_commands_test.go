package manager

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
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
	m := newBindingsService()
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
