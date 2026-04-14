package ally

import (
	"fmt"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// Code is the unique 8-character code that identifies an ally session.
type Code = string

// Session is an active cross-guild audio relay owned by one host guild.
// Guest guilds attach their speaker output channels via AddGuild; the host's
// relay goroutine calls Broadcast on every incoming packet.
// In AllyCaller mode guest guilds can also relay captured audio to all OTHER
// guilds via BroadcastFromGuild — their own guild is excluded to prevent
// speakers playing back audio to the users who originally produced it.
type Session struct {
	Code        Code
	HostGuildID snowflake.ID
	HostMode    guild.RaidMode

	done chan struct{}

	mu   sync.RWMutex
	outs map[snowflake.ID][]chan<- []byte // guildID → speaker chOut channels
}

func newSession(code Code, hostGuildID snowflake.ID, mode guild.RaidMode) *Session {
	return &Session{
		Code:        code,
		HostGuildID: hostGuildID,
		HostMode:    mode,
		done:        make(chan struct{}),
		outs:        make(map[snowflake.ID][]chan<- []byte),
	}
}

// broadcast sends pkt to every guild except excludeGuildID.
// Pass 0 to send to all guilds. Must be called with s.mu held for reading.
func (s *Session) broadcast(pkt []byte, excludeGuildID snowflake.ID) {
	for guildID, chs := range s.outs {
		if guildID == excludeGuildID {
			continue
		}
		for _, ch := range chs {
			select {
			case ch <- pkt:
			default:
			}
		}
	}
}

// Broadcast fans a packet out to every registered guild's speaker channels.
// Non-blocking: full channels drop the frame.
func (s *Session) Broadcast(pkt []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.broadcast(pkt, 0)
}

// BroadcastFromGuild fans a packet out to every guild except srcGuildID.
// Used by AllyCaller guests to relay captured audio without echoing it back
// to the guild that produced it.
// Non-blocking: full channels drop the frame.
func (s *Session) BroadcastFromGuild(srcGuildID snowflake.ID, pkt []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.broadcast(pkt, srcGuildID)
}

// AddGuild registers a guild's speaker output channels with this session.
func (s *Session) AddGuild(guildID snowflake.ID, chs []chan<- []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outs[guildID] = chs
}

// RemoveGuild detaches a guild's channels. The caller is responsible for
// closing the channels afterward.
func (s *Session) RemoveGuild(guildID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.outs, guildID)
}

// GuestGuildIDs returns the IDs of all guilds attached to this session except the host.
func (s *Session) GuestGuildIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]snowflake.ID, 0, len(s.outs))
	for id := range s.outs {
		if id != s.HostGuildID {
			ids = append(ids, id)
		}
	}
	return ids
}

// Done returns a channel closed when the host ends the session.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// close signals all guests that the host has ended the session.
func (s *Session) close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// Manager is the global registry of active ally sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[Code]*Session         // code → session
	byGuild  map[snowflake.ID]*Session // guildID (host or guest) → session
}

func NewManager() *Manager {
	return &Manager{
		sessions: make(map[Code]*Session),
		byGuild:  make(map[snowflake.ID]*Session),
	}
}

// Create registers a new session for hostGuildID using the supplied code and returns it.
// The caller is responsible for providing a stable, unique code (e.g. from the store).
func (m *Manager) Create(code Code, hostGuildID snowflake.ID, mode guild.RaidMode) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := newSession(code, hostGuildID, mode)
	m.sessions[s.Code] = s
	m.byGuild[hostGuildID] = s
	return s
}

// Join attaches guildID to the session identified by code.
func (m *Manager) Join(code Code, guildID snowflake.ID) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[code]
	if !ok {
		return nil, fmt.Errorf("session %q not found", code)
	}
	if s.HostGuildID == guildID {
		return nil, fmt.Errorf("cannot join your own session")
	}
	if _, already := m.byGuild[guildID]; already {
		return nil, fmt.Errorf("guild already has an active session")
	}
	m.byGuild[guildID] = s
	return s, nil
}

// GetByGuild returns the session the guild belongs to (as host or guest).
func (m *Manager) GetByGuild(guildID snowflake.ID) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byGuild[guildID]
	return s, ok
}

// RemoveGuest detaches a guest guild without affecting other participants.
func (m *Manager) RemoveGuest(guildID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byGuild, guildID)
}

// RemoveHost closes the session and removes all participants from the registry.
func (m *Manager) RemoveHost(hostGuildID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byGuild[hostGuildID]
	if !ok {
		return
	}
	s.close()
	delete(m.sessions, s.Code)
	delete(m.byGuild, hostGuildID)
	for guildID, sess := range m.byGuild {
		if sess == s {
			delete(m.byGuild, guildID)
		}
	}
}
