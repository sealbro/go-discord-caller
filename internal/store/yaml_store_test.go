package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

const (
	guild1 snowflake.ID = 100
	guild2 snowflake.ID = 200
	user1  snowflake.ID = 10
	user2  snowflake.ID = 20
	chan1  snowflake.ID = 1
	chan2  snowflake.ID = 2
	role1  snowflake.ID = 1000
	role2  snowflake.ID = 2000
)

func newTestStore(t *testing.T) *YAMLStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.yaml")
	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("NewYAMLStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// waitFlush closes the store and waits for the flush to complete, then reopens
// a fresh store from the same file so the caller can verify persisted state.
func reopen(t *testing.T, s *YAMLStore) *YAMLStore {
	t.Helper()
	path := s.path
	s.Close()
	s2, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("reopen YAMLStore: %v", err)
	}
	t.Cleanup(s2.Close)
	return s2
}

// TestYAMLStore_MissingFile verifies that a missing file is treated as empty.
func TestYAMLStore_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")
	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	defer s.Close()
	if _, ok := s.GetBoundChannel(guild1, user1); ok {
		t.Error("expected no channel bindings in empty store")
	}
}

// TestYAMLStore_InvalidFile verifies that a corrupt YAML file returns an error.
func TestYAMLStore_InvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte(":::invalid yaml:::{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewYAMLStore(path)
	if err == nil {
		t.Fatal("expected error for corrupt YAML, got nil")
	}
}

// TestYAMLStore_ChannelBindUnbind covers BindChannel, GetBoundChannel, UnbindChannel.
func TestYAMLStore_ChannelBindUnbind(t *testing.T) {
	s := newTestStore(t)

	s.BindChannel(guild1, user1, chan1)
	s.BindChannel(guild1, user2, chan2)

	if got, ok := s.GetBoundChannel(guild1, user1); !ok || got != chan1 {
		t.Errorf("GetBoundChannel guild1/user1 = %v,%v; want %v,true", got, ok, chan1)
	}
	if got, ok := s.GetBoundChannel(guild1, user2); !ok || got != chan2 {
		t.Errorf("GetBoundChannel guild1/user2 = %v,%v; want %v,true", got, ok, chan2)
	}

	s.UnbindChannel(guild1, user1)
	if _, ok := s.GetBoundChannel(guild1, user1); ok {
		t.Error("expected channel unbound after UnbindChannel")
	}
	// user2 unaffected
	if _, ok := s.GetBoundChannel(guild1, user2); !ok {
		t.Error("user2 binding should still exist after unrelated unbind")
	}
}

// TestYAMLStore_ChannelGuildIsolation verifies bindings are per-guild.
func TestYAMLStore_ChannelGuildIsolation(t *testing.T) {
	s := newTestStore(t)
	s.BindChannel(guild1, user1, chan1)
	s.BindChannel(guild2, user1, chan2)

	if got, _ := s.GetBoundChannel(guild1, user1); got != chan1 {
		t.Errorf("guild1 channel = %v; want %v", got, chan1)
	}
	if got, _ := s.GetBoundChannel(guild2, user1); got != chan2 {
		t.Errorf("guild2 channel = %v; want %v", got, chan2)
	}
}

// TestYAMLStore_RoleBindUnbind covers BindRole, GetBoundRole, UnbindRole.
func TestYAMLStore_RoleBindUnbind(t *testing.T) {
	s := newTestStore(t)

	s.BindRole(guild1, RoleTypeCaller, role1)
	s.BindRole(guild1, RoleTypeManager, role2)

	if got, ok := s.GetBoundRole(guild1, RoleTypeCaller); !ok || got != role1 {
		t.Errorf("GetBoundRole caller = %v,%v; want %v,true", got, ok, role1)
	}
	if got, ok := s.GetBoundRole(guild1, RoleTypeManager); !ok || got != role2 {
		t.Errorf("GetBoundRole manager = %v,%v; want %v,true", got, ok, role2)
	}

	s.UnbindRole(guild1, RoleTypeCaller)
	if _, ok := s.GetBoundRole(guild1, RoleTypeCaller); ok {
		t.Error("expected role unbound after UnbindRole")
	}
	if _, ok := s.GetBoundRole(guild1, RoleTypeManager); !ok {
		t.Error("manager role should still exist after unrelated unbind")
	}
}

// TestYAMLStore_AllyCode covers GetOrCreateAllyCode idempotency and GetAllyCode.
func TestYAMLStore_AllyCode(t *testing.T) {
	s := newTestStore(t)

	code1 := s.GetOrCreateAllyCode(guild1)
	if len(code1) != 8 {
		t.Errorf("ally code length = %d; want 8", len(code1))
	}
	// Second call must return the same code.
	if got := s.GetOrCreateAllyCode(guild1); got != code1 {
		t.Errorf("GetOrCreateAllyCode not idempotent: %q vs %q", got, code1)
	}

	code2 := s.GetOrCreateAllyCode(guild2)
	if code1 == code2 {
		t.Errorf("expected distinct codes for distinct guilds, both got %q", code1)
	}

	if got, ok := s.GetAllyCode(guild1); !ok || got != code1 {
		t.Errorf("GetAllyCode = %q,%v; want %q,true", got, ok, code1)
	}
	if _, ok := s.GetAllyCode(999); ok {
		t.Error("expected no code for unknown guild")
	}
}

// TestYAMLStore_Persistence verifies state survives Close + reopen.
func TestYAMLStore_Persistence(t *testing.T) {
	s := newTestStore(t)

	s.BindChannel(guild1, user1, chan1)
	s.BindRole(guild1, RoleTypeCaller, role1)
	code := s.GetOrCreateAllyCode(guild1)
	s.BindLocale(guild1, "ru")

	s2 := reopen(t, s)

	if got, ok := s2.GetBoundChannel(guild1, user1); !ok || got != chan1 {
		t.Errorf("channel not persisted: %v,%v", got, ok)
	}
	if got, ok := s2.GetBoundRole(guild1, RoleTypeCaller); !ok || got != role1 {
		t.Errorf("role not persisted: %v,%v", got, ok)
	}
	if got, ok := s2.GetAllyCode(guild1); !ok || got != code {
		t.Errorf("ally code not persisted: %q,%v", got, ok)
	}
	if got, ok := s2.GetLocale(guild1); !ok || got != "ru" {
		t.Errorf("locale not persisted: %q,%v", got, ok)
	}
}

// TestYAMLStore_LocaleBindUnbind covers BindLocale, GetLocale, UnbindLocale,
// and the "BindLocale with empty string clears the pin" shortcut.
func TestYAMLStore_LocaleBindUnbind(t *testing.T) {
	s := newTestStore(t)

	// Initially unset.
	if _, ok := s.GetLocale(guild1); ok {
		t.Error("expected no locale set on a fresh store")
	}

	// Bind a real locale.
	s.BindLocale(guild1, "ru")
	if got, ok := s.GetLocale(guild1); !ok || got != "ru" {
		t.Errorf("GetLocale after BindLocale(ru) = %q,%v; want %q,true", got, ok, "ru")
	}

	// Overwrite with a different locale.
	s.BindLocale(guild1, "de")
	if got, ok := s.GetLocale(guild1); !ok || got != "de" {
		t.Errorf("GetLocale after BindLocale(de) = %q,%v; want %q,true", got, ok, "de")
	}

	// UnbindLocale clears the pin.
	s.UnbindLocale(guild1)
	if _, ok := s.GetLocale(guild1); ok {
		t.Error("expected locale cleared after UnbindLocale")
	}

	// BindLocale("") is equivalent to UnbindLocale (documented shortcut).
	s.BindLocale(guild1, "ru")
	s.BindLocale(guild1, "")
	if _, ok := s.GetLocale(guild1); ok {
		t.Error("expected BindLocale(\"\") to clear the pin")
	}
}

// TestYAMLStore_LocaleGuildIsolation verifies locale pins are per-guild.
func TestYAMLStore_LocaleGuildIsolation(t *testing.T) {
	s := newTestStore(t)

	s.BindLocale(guild1, "ru")
	s.BindLocale(guild2, "de")

	if got, _ := s.GetLocale(guild1); got != "ru" {
		t.Errorf("guild1 locale = %q; want %q", got, "ru")
	}
	if got, _ := s.GetLocale(guild2); got != "de" {
		t.Errorf("guild2 locale = %q; want %q", got, "de")
	}

	// Unbinding guild1 must not affect guild2.
	s.UnbindLocale(guild1)
	if _, ok := s.GetLocale(guild1); ok {
		t.Error("guild1 locale should be cleared")
	}
	if got, _ := s.GetLocale(guild2); got != "de" {
		t.Errorf("guild2 locale unexpectedly changed to %q; want %q", got, "de")
	}
}

// TestYAMLStore_LocaleUnbindPersisted verifies that clearing a locale survives
// reopen (the unbind itself is persisted, not just the absence).
func TestYAMLStore_LocaleUnbindPersisted(t *testing.T) {
	s := newTestStore(t)

	s.BindLocale(guild1, "ru")
	s.BindLocale(guild2, "de")
	s.UnbindLocale(guild1)

	s2 := reopen(t, s)

	if _, ok := s2.GetLocale(guild1); ok {
		t.Error("unbound locale should not be persisted")
	}
	if got, ok := s2.GetLocale(guild2); !ok || got != "de" {
		t.Errorf("surviving locale = %q,%v; want %q,true", got, ok, "de")
	}
}

// TestYAMLStore_PersistenceUnbind verifies that unbinds are persisted too.
func TestYAMLStore_PersistenceUnbind(t *testing.T) {
	s := newTestStore(t)

	s.BindChannel(guild1, user1, chan1)
	s.BindChannel(guild1, user2, chan2)
	s.UnbindChannel(guild1, user1)

	s2 := reopen(t, s)

	if _, ok := s2.GetBoundChannel(guild1, user1); ok {
		t.Error("unbound channel should not be persisted")
	}
	if _, ok := s2.GetBoundChannel(guild1, user2); !ok {
		t.Error("surviving channel binding should be persisted")
	}
}

// TestYAMLStore_MultiGuildPersistence verifies multiple guilds round-trip cleanly.
func TestYAMLStore_MultiGuildPersistence(t *testing.T) {
	s := newTestStore(t)

	s.BindChannel(guild1, user1, chan1)
	s.BindChannel(guild2, user2, chan2)
	s.BindRole(guild1, RoleTypeCaller, role1)
	s.BindRole(guild2, RoleTypeManager, role2)

	s2 := reopen(t, s)

	if got, _ := s2.GetBoundChannel(guild1, user1); got != chan1 {
		t.Errorf("guild1 channel: got %v, want %v", got, chan1)
	}
	if got, _ := s2.GetBoundChannel(guild2, user2); got != chan2 {
		t.Errorf("guild2 channel: got %v, want %v", got, chan2)
	}
	if got, _ := s2.GetBoundRole(guild1, RoleTypeCaller); got != role1 {
		t.Errorf("guild1 role: got %v, want %v", got, role1)
	}
	if got, _ := s2.GetBoundRole(guild2, RoleTypeManager); got != role2 {
		t.Errorf("guild2 role: got %v, want %v", got, role2)
	}
}

// TestYAMLStore_AtomicWrite verifies the file is never partially written by
// checking it is valid YAML immediately after Close.
func TestYAMLStore_AtomicWrite(t *testing.T) {
	s := newTestStore(t)
	path := s.path

	for i := range 50 {
		s.BindChannel(guild1, snowflake.ID(i+1), chan1)
	}
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file after close: %v", err)
	}
	if err := unmarshalYAML(data); err != nil {
		t.Errorf("file is not valid YAML after close: %v", err)
	}
}

// TestYAMLStore_DebounceFlush verifies the debounce timer flushes to disk
// without an explicit Close.
func TestYAMLStore_DebounceFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debounce.yaml")
	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.BindChannel(guild1, user1, chan1)

	// Wait longer than saveDebounce for the background flush to fire.
	deadline := time.Now().Add(saveDebounce + 1500*time.Millisecond)
	for time.Now().Before(deadline) {
		s2, err := NewYAMLStore(path)
		if err == nil {
			_, ok := s2.GetBoundChannel(guild1, user1)
			s2.Close()
			if ok {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("file was not flushed within debounce window")
}

// unmarshalYAML validates data by loading it through a temporary YAMLStore.
func unmarshalYAML(data []byte) error {
	tmp, err := os.CreateTemp("", "*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	tmp.Close()
	ts := &YAMLStore{
		path:       tmp.Name(),
		channels:   make(map[channelKey]snowflake.ID),
		roles:      make(map[roleKey]snowflake.ID),
		relayCodes: make(map[snowflake.ID]string),
		locales:    make(map[snowflake.ID]string),
		dirtyCh:    make(chan struct{}, 1),
		done:       make(chan struct{}),
		flushed:    make(chan struct{}),
	}
	return ts.load()
}
