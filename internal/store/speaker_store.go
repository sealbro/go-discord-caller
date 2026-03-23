package store

import (
	"fmt"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// SpeakerStore manages speaker bot records.
type SpeakerStore interface {
	AddSpeaker(s *domain.Speaker)
	GetSpeaker(id snowflake.ID) (*domain.Speaker, bool)
	GetSpeakerByToken(token string) (*domain.Speaker, bool)
	UpdateSpeaker(s *domain.Speaker) error
	ListAllSpeakers() []*domain.Speaker
	RemoveSpeaker(id snowflake.ID)
}

// InMemorySpeakerStore is a thread-safe in-memory implementation of SpeakerStore.
type InMemorySpeakerStore struct {
	mu         sync.RWMutex
	speakers   map[snowflake.ID]*domain.Speaker
	tokenIndex map[string]snowflake.ID // botToken -> speakerID
}

// NewInMemorySpeakerStore creates a new empty InMemorySpeakerStore.
func NewInMemorySpeakerStore() *InMemorySpeakerStore {
	return &InMemorySpeakerStore{
		speakers:   make(map[snowflake.ID]*domain.Speaker),
		tokenIndex: make(map[string]snowflake.ID),
	}
}

func (s *InMemorySpeakerStore) AddSpeaker(sp *domain.Speaker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakers[sp.ID] = sp
	s.tokenIndex[sp.BotToken] = sp.ID
}

func (s *InMemorySpeakerStore) GetSpeaker(id snowflake.ID) (*domain.Speaker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sp, ok := s.speakers[id]
	return sp, ok
}

func (s *InMemorySpeakerStore) GetSpeakerByToken(token string) (*domain.Speaker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokenIndex[token]
	if !ok {
		return nil, false
	}
	sp, ok := s.speakers[id]
	return sp, ok
}

func (s *InMemorySpeakerStore) UpdateSpeaker(sp *domain.Speaker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.speakers[sp.ID]; !ok {
		return fmt.Errorf("speaker %s not found", sp.ID)
	}
	s.speakers[sp.ID] = sp
	return nil
}

func (s *InMemorySpeakerStore) ListAllSpeakers() []*domain.Speaker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.Speaker, 0, len(s.speakers))
	for _, sp := range s.speakers {
		result = append(result, sp)
	}
	return result
}

func (s *InMemorySpeakerStore) RemoveSpeaker(id snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.speakers[id]; ok {
		delete(s.tokenIndex, sp.BotToken)
		delete(s.speakers, id)
	}
}
