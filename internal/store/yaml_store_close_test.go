package store

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestYAMLStore_WriteAfterCloseIsPersisted(t *testing.T) {
	s := newTestStore(t)
	s.Close()

	s.BindChannel(guild1, user1, chan1)

	s2 := reopen(t, s)
	if got, ok := s2.GetBoundChannel(guild1, user1); !ok || got != chan1 {
		t.Errorf("binding written after Close was lost: got %v, %v", got, ok)
	}
}

func TestYAMLStore_DuplicateRelayCodesOnLoadAreMadeUnique(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.yaml")
	data := "guilds:\n  - guild_id: 100\n    ally_code: AAAAAAAA\n  - guild_id: 200\n    ally_code: AAAAAAAA\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("NewYAMLStore: %v", err)
	}
	t.Cleanup(s.Close)

	first, _ := s.GetAllyCode(guild1)
	second, _ := s.GetAllyCode(guild2)
	if first == second {
		t.Errorf("two guilds share relay code %q after load", first)
	}
	if first != "AAAAAAAA" {
		t.Errorf("first guild lost its code: %q", first)
	}
}

func TestYAMLStore_CloseIsSafeFromSeveralGoroutines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.yaml")
	for range 200 {
		s, err := NewYAMLStore(path)
		if err != nil {
			t.Fatalf("NewYAMLStore: %v", err)
		}
		var writes atomic.Int32
		s.mu.Lock()
		s.writeFile = func(string, []byte) error {
			writes.Add(1)
			return nil
		}
		s.mu.Unlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				<-start
				s.Close()
			})
		}
		close(start)
		wg.Wait()
		if n := writes.Load(); n != 1 {
			t.Fatalf("concurrent Closes flushed %d times, want once", n)
		}
	}
}
