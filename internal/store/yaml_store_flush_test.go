package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// A flush ends in an fsync, which on a busy or slow disk takes long enough to
// notice. Holding the store's lock across it stalls every lookup made
// meanwhile, and those are made from Discord event handlers.
func TestYAMLStore_FlushDoesNotBlockAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.yaml")
	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("NewYAMLStore: %v", err)
	}

	writing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.writeFile = func(path string, data []byte) error {
		once.Do(func() { close(writing) })
		<-release
		return writeFileAtomic(path, data)
	}
	s.mu.Unlock()

	s.BindChannel(1, 2, 3)
	select {
	case <-writing:
	case <-time.After(10 * saveDebounce):
		t.Fatal("the debounced flush never started")
	}

	accessed := make(chan struct{})
	go func() {
		defer close(accessed)
		s.BindChannel(1, 4, 5)
		s.GetBoundChannel(1, 2)
	}()
	select {
	case <-accessed:
	case <-time.After(2 * time.Second):
		t.Error("a bind and a lookup are still blocked behind a flush that is writing to disk")
	}

	close(release)
	<-accessed
	s.Close()

	reloaded, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer reloaded.Close()
	for _, userID := range []uint64{2, 4} {
		if _, ok := reloaded.GetBoundChannel(1, snowflake.ID(userID)); !ok {
			t.Errorf("binding of bot %d was not persisted", userID)
		}
	}
}

// With the write outside the lock, two flushes can reach the disk in either
// order; the older state must not win.
func TestYAMLStore_OlderSnapshotNeverOverwritesNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.yaml")
	s, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("NewYAMLStore: %v", err)
	}

	s.mu.Lock()
	older, err := s.snapshot()
	s.channels[botKey{userID: 2, guildID: 1}] = 3
	newer, err2 := s.snapshot()
	s.mu.Unlock()
	if err != nil || err2 != nil {
		t.Fatalf("snapshot: %v, %v", err, err2)
	}

	if err := s.persist(newer); err != nil {
		t.Fatalf("persist newer: %v", err)
	}
	if err := s.persist(older); err != nil {
		t.Fatalf("persist older: %v", err)
	}

	reloaded, err := NewYAMLStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer reloaded.Close()
	if _, ok := reloaded.GetBoundChannel(1, 2); !ok {
		t.Error("a snapshot taken before the binding overwrote the one taken after it")
	}
	s.Close()
}
