package store

import "testing"

func TestInMemoryStoreRoles(t *testing.T) {
	s := NewInMemoryStore()
	defer s.Close()

	s.BindRole(guild1, RoleTypeManager, role1)
	if got, ok := s.GetBoundRole(guild1, RoleTypeManager); !ok || got != role1 {
		t.Fatalf("GetBoundRole = %v, %v; want %v, true", got, ok, role1)
	}
	if _, ok := s.GetBoundRole(guild1, RoleTypeCaller); ok {
		t.Fatal("caller role bound without BindRole")
	}

	s.UnbindRole(guild1, RoleTypeManager)
	if _, ok := s.GetBoundRole(guild1, RoleTypeManager); ok {
		t.Fatal("manager role still bound after UnbindRole")
	}
}

func TestInMemoryStoreAllyCode(t *testing.T) {
	s := NewInMemoryStore()

	if _, ok := s.GetAllyCode(guild1); ok {
		t.Fatal("ally code present before creation")
	}
	code := s.GetOrCreateAllyCode(guild1)
	if got, ok := s.GetAllyCode(guild1); !ok || got != code {
		t.Fatalf("GetAllyCode = %q, %v; want %q, true", got, ok, code)
	}
	if again := s.GetOrCreateAllyCode(guild1); again != code {
		t.Fatalf("GetOrCreateAllyCode changed code: %q -> %q", code, again)
	}
	if other := s.GetOrCreateAllyCode(guild2); other == code {
		t.Fatalf("two guilds share ally code %q", code)
	}
}

func TestInMemoryStoreLocale(t *testing.T) {
	s := NewInMemoryStore()

	if _, ok := s.GetLocale(guild1); ok {
		t.Fatal("locale present before BindLocale")
	}

	s.BindLocale(guild1, "ru")
	if got, ok := s.GetLocale(guild1); !ok || got != "ru" {
		t.Fatalf("GetLocale = %q, %v; want ru, true", got, ok)
	}

	s.BindLocale(guild1, "")
	if _, ok := s.GetLocale(guild1); ok {
		t.Fatal("empty BindLocale did not clear the locale")
	}

	s.BindLocale(guild1, "de")
	s.UnbindLocale(guild1)
	if _, ok := s.GetLocale(guild1); ok {
		t.Fatal("locale still set after UnbindLocale")
	}
}

func TestInMemoryStoreSpeakerEnabled(t *testing.T) {
	s := NewInMemoryStore()

	if !s.IsSpeakerEnabled(guild1, user1) {
		t.Fatal("speaker disabled by default")
	}

	s.SetSpeakerEnabled(guild1, user1, false)
	if s.IsSpeakerEnabled(guild1, user1) {
		t.Fatal("speaker enabled after disabling")
	}
	if !s.IsSpeakerEnabled(guild2, user1) {
		t.Fatal("disabling in one guild leaked into another")
	}

	s.SetSpeakerEnabled(guild1, user1, true)
	if !s.IsSpeakerEnabled(guild1, user1) {
		t.Fatal("speaker still disabled after re-enabling")
	}
}
