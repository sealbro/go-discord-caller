package store

import (
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// SessionStore manages voice raid sessions keyed by guild ID.
type SessionStore interface {
	GetSession(guildID snowflake.ID) (*domain.VoiceSession, bool)
	SetSession(s *domain.VoiceSession)
	DeleteSession(guildID snowflake.ID)
	ListSessions() []*domain.VoiceSession
}

// InMemorySessionStore is a thread-safe in-memory implementation of SessionStore.
type InMemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[snowflake.ID]*domain.VoiceSession
}

// NewInMemorySessionStore creates a new empty InMemorySessionStore.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{
		sessions: make(map[snowflake.ID]*domain.VoiceSession),
	}
}

func (s *InMemorySessionStore) GetSession(guildID snowflake.ID) (*domain.VoiceSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[guildID]
	return sess, ok
}

func (s *InMemorySessionStore) SetSession(sess *domain.VoiceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.GuildID] = sess
}

func (s *InMemorySessionStore) DeleteSession(guildID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, guildID)
}

func (s *InMemorySessionStore) ListSessions() []*domain.VoiceSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.VoiceSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		result = append(result, sess)
	}
	return result
}
