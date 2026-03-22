package store

import (
	"fmt"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
)

// channelKey is the composite key used to look up a voice-channel binding.
type channelKey struct {
	userID  snowflake.ID
	guildID snowflake.ID
}

// Store is the persistence layer for speakers, sessions, and role bindings.
type Store interface {
	// Speaker CRUD
	AddSpeaker(s *domain.Speaker)
	GetSpeaker(id snowflake.ID) (*domain.Speaker, bool)
	GetSpeakerByToken(token string) (*domain.Speaker, bool)
	UpdateSpeaker(s *domain.Speaker) error
	ListSpeakers(guildID snowflake.ID) []*domain.Speaker
	RemoveSpeaker(id snowflake.ID)

	// Channel binding — keyed by (userID, guildID) for both speaker bots and the owner bot.
	BindChannel(guildID snowflake.ID, userID snowflake.ID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)

	// Role binding
	BindRole(guildID, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID)
	GetBoundRole(guildID snowflake.ID) (snowflake.ID, bool)

	// Session management
	GetSession(guildID snowflake.ID) (*domain.VoiceSession, bool)
	SetSession(s *domain.VoiceSession)
	DeleteSession(guildID snowflake.ID)
	ListSessions() []*domain.VoiceSession
}

// InMemoryStore is a thread-safe in-memory implementation of Store.
type InMemoryStore struct {
	mu         sync.RWMutex
	speakers   map[snowflake.ID]*domain.Speaker
	tokenIndex map[string]snowflake.ID       // botToken -> speakerID
	roles      map[snowflake.ID]snowflake.ID // guildID -> roleID
	channels   map[channelKey]snowflake.ID   // (userID, guildID) -> channelID
	sessions   map[snowflake.ID]*domain.VoiceSession
}

// NewInMemoryStore creates a new empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		speakers:   make(map[snowflake.ID]*domain.Speaker),
		tokenIndex: make(map[string]snowflake.ID),
		roles:      make(map[snowflake.ID]snowflake.ID),
		channels:   make(map[channelKey]snowflake.ID),
		sessions:   make(map[snowflake.ID]*domain.VoiceSession),
	}
}

func (s *InMemoryStore) AddSpeaker(sp *domain.Speaker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakers[sp.ID] = sp
	s.tokenIndex[sp.BotToken] = sp.ID
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
		if _, ok := sp.Guilds[guildID]; ok {
			result = append(result, sp)
		}
	}
	return result
}

func (s *InMemoryStore) GetSpeakerByToken(token string) (*domain.Speaker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokenIndex[token]
	if !ok {
		return nil, false
	}
	sp, ok := s.speakers[id]
	return sp, ok
}

func (s *InMemoryStore) RemoveSpeaker(id snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.speakers[id]; ok {
		delete(s.tokenIndex, sp.BotToken)
		delete(s.speakers, id)
	}
}

func (s *InMemoryStore) BindChannel(guildID snowflake.ID, userID snowflake.ID, channelID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[channelKey{userID, guildID}] = channelID
}

func (s *InMemoryStore) UnbindChannel(guildID, userID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.channels, channelKey{userID, guildID})
}

func (s *InMemoryStore) GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[channelKey{userID, guildID}]
	return ch, ok
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

func (s *InMemoryStore) ListSessions() []*domain.VoiceSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.VoiceSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		result = append(result, sess)
	}
	return result
}
