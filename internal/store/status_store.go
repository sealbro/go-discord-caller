package store

import (
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// StatusStore persists per-guild configuration (enabled speakers, bound channels, role).
type StatusStore interface {
	GetStatus(guildID snowflake.ID) *domain.GuildStatus
	SetStatus(s *domain.GuildStatus)
	ListStatuses() []*domain.GuildStatus
}

// InMemoryStatusStore is a thread-safe in-memory implementation of StatusStore.
type InMemoryStatusStore struct {
	mu       sync.RWMutex
	statuses map[snowflake.ID]*domain.GuildStatus
}

// NewInMemoryStatusStore creates a new empty InMemoryStatusStore.
func NewInMemoryStatusStore() *InMemoryStatusStore {
	return &InMemoryStatusStore{
		statuses: make(map[snowflake.ID]*domain.GuildStatus),
	}
}

func (s *InMemoryStatusStore) GetStatus(guildID snowflake.ID) *domain.GuildStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, _ := s.statuses[guildID]
	return st
}

func (s *InMemoryStatusStore) SetStatus(st *domain.GuildStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[st.GuildID] = st
}

func (s *InMemoryStatusStore) ListStatuses() []*domain.GuildStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.GuildStatus, 0, len(s.statuses))
	for _, st := range s.statuses {
		result = append(result, st)
	}
	return result
}
