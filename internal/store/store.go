package store

import (
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// channelKey is the composite key used to look up a voice-channel binding.
type channelKey struct {
	userID  snowflake.ID
	guildID snowflake.ID
}

// Store is the persistence layer for channel and role bindings.
type Store interface {
	// Channel binding — keyed by (userID, guildID) for both speaker bots and the owner bot.
	BindChannel(guildID snowflake.ID, userID snowflake.ID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)

	// Role binding
	BindRole(guildID, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID)
	GetBoundRole(guildID snowflake.ID) (snowflake.ID, bool)
}

// InMemoryStore is a thread-safe in-memory implementation of Store.
type InMemoryStore struct {
	mu       sync.RWMutex
	roles    map[snowflake.ID]snowflake.ID // guildID -> roleID
	channels map[channelKey]snowflake.ID   // (userID, guildID) -> channelID
}

// NewInMemoryStore creates a new empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		roles:    make(map[snowflake.ID]snowflake.ID),
		channels: make(map[channelKey]snowflake.ID),
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
