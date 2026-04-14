package store

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// RoleType distinguishes the two role kinds the bot tracks per guild.
type RoleType string

const (
	// RoleTypeCaller is the role whose members' voice is captured and relayed.
	RoleTypeCaller RoleType = "caller"
	// RoleTypeManager is the role whose members are allowed to setup, start and stop the bot.
	RoleTypeManager RoleType = "manager"
)

// channelKey is the composite key for a voice-channel binding.
type channelKey struct {
	userID  snowflake.ID
	guildID snowflake.ID
}

// roleKey is the composite key for a role binding.
type roleKey struct {
	guildID  snowflake.ID
	roleType RoleType
}

// Store is the persistence layer for channel and role bindings.
type Store interface {
	BindChannel(guildID, userID, channelID snowflake.ID)
	UnbindChannel(guildID, userID snowflake.ID)
	GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)

	BindRole(guildID snowflake.ID, roleType RoleType, roleID snowflake.ID)
	UnbindRole(guildID snowflake.ID, roleType RoleType)
	GetBoundRole(guildID snowflake.ID, roleType RoleType) (snowflake.ID, bool)

	// GetOrCreateAllyCode returns the persistent ally code for guildID.
	// If none exists it generates, persists, and returns a new one.
	GetOrCreateAllyCode(guildID snowflake.ID) string
	// GetAllyCode returns the stored ally code for guildID without creating one.
	GetAllyCode(guildID snowflake.ID) (string, bool)

	// Close flushes any pending writes and releases resources.
	Close()
}

// generateAllyCode creates a cryptographically random 8-character uppercase code.
func generateAllyCode() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:8]
}

// uniqueAllyCode generates an ally code that does not already appear as a
// value in existing. Must be called with the store's write lock held.
func uniqueAllyCode(existing map[snowflake.ID]string) string {
	used := make(map[string]struct{}, len(existing))
	for _, c := range existing {
		used[c] = struct{}{}
	}
	for {
		code := generateAllyCode()
		if _, collision := used[code]; !collision {
			return code
		}
	}
}

// InMemoryStore is a thread-safe in-memory implementation of Store.
type InMemoryStore struct {
	mu         sync.RWMutex
	channels   map[channelKey]snowflake.ID // (userID, guildID) -> channelID
	roles      map[roleKey]snowflake.ID    // (guildID, roleType) -> roleID
	relayCodes map[snowflake.ID]string     // guildID -> relay code
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		channels:   make(map[channelKey]snowflake.ID),
		roles:      make(map[roleKey]snowflake.ID),
		relayCodes: make(map[snowflake.ID]string),
	}
}

func (s *InMemoryStore) BindChannel(guildID, userID, channelID snowflake.ID) {
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

func (s *InMemoryStore) BindRole(guildID snowflake.ID, roleType RoleType, roleID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[roleKey{guildID, roleType}] = roleID
}

func (s *InMemoryStore) UnbindRole(guildID snowflake.ID, roleType RoleType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles, roleKey{guildID, roleType})
}

func (s *InMemoryStore) GetBoundRole(guildID snowflake.ID, roleType RoleType) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roleID, ok := s.roles[roleKey{guildID, roleType}]
	return roleID, ok
}

func (s *InMemoryStore) GetOrCreateAllyCode(guildID snowflake.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code, ok := s.relayCodes[guildID]; ok {
		return code
	}
	code := uniqueAllyCode(s.relayCodes)
	s.relayCodes[guildID] = code
	return code
}

func (s *InMemoryStore) GetAllyCode(guildID snowflake.ID) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	code, ok := s.relayCodes[guildID]
	return code, ok
}

func (s *InMemoryStore) Close() {}
