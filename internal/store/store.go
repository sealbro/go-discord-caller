package store

import (
	"fmt"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// Store is the persistence layer for speakers, sessions, and role bindings.
type Store interface {
	// Speaker CRUD
	AddSpeaker(s *domain.Speaker)
	GetSpeaker(id snowflake.ID) (*domain.Speaker, bool)
	UpdateSpeaker(s *domain.Speaker) error
	ListSpeakers(guildID snowflake.ID) []*domain.Speaker
	RemoveSpeaker(id snowflake.ID)

	// Channel binding
	BindChannel(speakerID, channelID snowflake.ID) error
	UnbindChannel(speakerID snowflake.ID)

	// Role binding
	BindRole(guildID, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID)
	GetBoundRole(guildID snowflake.ID) (snowflake.ID, bool)

	// Session management
	GetSession(guildID snowflake.ID) (*domain.VoiceSession, bool)
	SetSession(s *domain.VoiceSession)
	DeleteSession(guildID snowflake.ID)
}

// InMemoryStore is a thread-safe in-memory implementation of Store.
type InMemoryStore struct {
	mu       sync.RWMutex
	speakers map[snowflake.ID]*domain.Speaker
	roles    map[snowflake.ID]snowflake.ID // guildID -> roleID
	sessions map[snowflake.ID]*domain.VoiceSession
}

// NewInMemoryStore creates a new empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		speakers: make(map[snowflake.ID]*domain.Speaker),
		roles:    make(map[snowflake.ID]snowflake.ID),
		sessions: make(map[snowflake.ID]*domain.VoiceSession),
	}
}

func (s *InMemoryStore) AddSpeaker(sp *domain.Speaker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakers[sp.ID] = sp
}

func (s *InMemoryStore) GetSpeaker(id snowflake.ID) (*domain.Speaker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sp, ok := s.speakers[id]
	return sp, ok
}

func (s *InMemoryStore) UpdateSpeaker(sp *domain.Speaker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.speakers[sp.ID]; !ok {
		return fmt.Errorf("speaker %s not found", sp.ID)
	}
	s.speakers[sp.ID] = sp
	return nil
}

func (s *InMemoryStore) ListSpeakers(guildID snowflake.ID) []*domain.Speaker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.Speaker
	for _, sp := range s.speakers {
		if sp.GuildID == guildID {
			result = append(result, sp)
		}
	}
	return result
}

func (s *InMemoryStore) RemoveSpeaker(id snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.speakers, id)
}

func (s *InMemoryStore) BindChannel(speakerID, channelID snowflake.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.speakers[speakerID]
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}
	sp.BoundChannelID = &channelID
	return nil
}

func (s *InMemoryStore) UnbindChannel(speakerID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.speakers[speakerID]; ok {
		sp.BoundChannelID = nil
	}
}

func (s *InMemoryStore) BindRole(guildID, roleID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[guildID] = roleID
}

func (s *InMemoryStore) UnbindRole(guildID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles, guildID)
}

func (s *InMemoryStore) GetBoundRole(guildID snowflake.ID) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roleID, ok := s.roles[guildID]
	return roleID, ok
}

func (s *InMemoryStore) GetSession(guildID snowflake.ID) (*domain.VoiceSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[guildID]
	return sess, ok
}

func (s *InMemoryStore) SetSession(sess *domain.VoiceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.GuildID] = sess
}

func (s *InMemoryStore) DeleteSession(guildID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, guildID)
}
