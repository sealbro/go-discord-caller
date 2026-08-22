package ally

import (
	"fmt"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
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

	mu        sync.RWMutex
	outs      map[snowflake.ID][]chan<- []byte // guildID → speaker chOut channels
	guests    map[snowflake.ID]struct{}        // guest guild IDs (host not included)
	capturing map[snowflake.ID]struct{}        // guilds that may broadcast into the session
	observers map[snowflake.ID]func()          // guildID → membership-change callback
}

func newSession(code Code, hostGuildID snowflake.ID, mode guild.RaidMode) *Session {
	return &Session{
		Code:        code,
		HostGuildID: hostGuildID,
		HostMode:    mode,
		done:        make(chan struct{}),
		outs:        make(map[snowflake.ID][]chan<- []byte),
		guests:      make(map[snowflake.ID]struct{}),
		capturing:   make(map[snowflake.ID]struct{}),
		observers:   make(map[snowflake.ID]func()),
	}
}

// broadcast sends pkt to every guild except excludeGuildID.
// Pass 0 to send to all guilds. Must be called with s.mu held for reading.
// Takes ownership of pkt: each channel receives its own copy so that
// VoiceProviders can independently return their buffer to the pool via
// PutEncodedFrame. The original pkt is returned to the pool at the end.
//
// LOCK-ORDERING CONTRACT: Send-to-closed-channel would panic here. The
// `select case ch <- buf: default:` only protects against full channels,
// NOT closed channels. Safety relies on the following invariant in
// session teardown:
//
//  1. `RemoveGuild(guildID)` takes s.mu.Lock(), which blocks until every
//     in-flight broadcast (holding RLock) completes.
//  2. ONLY AFTER RemoveGuild returns may the caller close the channels.
//
// Both callers in `manager/voice_raid.go` (host StopVoiceRaid / guest
// JoinSession defer) follow this order. If you add a new caller that
// owns channels registered via AddGuild, preserve this ordering.
func (s *Session) broadcast(pkt []byte, excludeGuildID snowflake.ID) {
	for guildID, chs := range s.outs {
		if guildID == excludeGuildID {
			continue
		}
		for _, ch := range chs {
			buf := opus.CopyOpusFrame(pkt)
			select {
			case ch <- buf:
			default:
				opus.PutEncodedFrame(buf)
			}
		}
	}
	opus.PutEncodedFrame(pkt)
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
	s.outs[guildID] = chs
	if guildID != s.HostGuildID {
		s.guests[guildID] = struct{}{}
	}
	fns := s.observersLocked()
	s.mu.Unlock()
	notify(fns)
}

// SetCapturing marks guildID as a guild that may broadcast captured audio into
// the session. Listener-only guests never call it, so their peers keep the
// cheap copy-mode route. Cleared by RemoveGuild.
func (s *Session) SetCapturing(guildID snowflake.ID) {
	s.mu.Lock()
	s.capturing[guildID] = struct{}{}
	fns := s.observersLocked()
	s.mu.Unlock()
	notify(fns)
}

// HasCapturingPeers reports whether any guild OTHER than guildID may broadcast
// into the session — i.e. whether guildID's relay input can carry audio.
// Destinations fed by the relay must run their mixer while this holds, since
// the relay is not a router-visible source (see router.DestSlot.RelayFeed).
func (s *Session) HasCapturingPeers(guildID snowflake.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id := range s.capturing {
		if id != guildID {
			return true
		}
	}
	return false
}

// SetRouteObserver registers fn to run whenever the set of attached or
// capturing guilds changes, so guildID's auto-router can re-evaluate its
// relay-fed destinations. Removed by RemoveGuild.
//
// fn is invoked on its own goroutine with s.mu released — it is free to call
// back into the session (HasCapturingPeers) without deadlocking.
func (s *Session) SetRouteObserver(guildID snowflake.ID, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers[guildID] = fn
}

// observersLocked snapshots the registered callbacks. Caller must hold s.mu.
func (s *Session) observersLocked() []func() {
	if len(s.observers) == 0 {
		return nil
	}
	fns := make([]func(), 0, len(s.observers))
	for _, fn := range s.observers {
		fns = append(fns, fn)
	}
	return fns
}

// notify runs each membership callback on its own goroutine. Never called with
// s.mu held.
func notify(fns []func()) {
	for _, fn := range fns {
		go fn()
	}
}

// RemoveGuild detaches a guild's channels. The caller is responsible for
// closing the channels afterward.
//
// Must be called BEFORE closing the channels — see the LOCK-ORDERING
// CONTRACT on `broadcast`. Acquiring s.mu.Lock() here blocks until every
// in-flight broadcast that captured this guild's channel slice has
// finished sending, after which it is safe to close the channels.
func (s *Session) RemoveGuild(guildID snowflake.ID) {
	s.mu.Lock()
	delete(s.outs, guildID)
	delete(s.guests, guildID)
	delete(s.capturing, guildID)
	delete(s.observers, guildID)
	fns := s.observersLocked()
	s.mu.Unlock()
	notify(fns)
}

// GuestGuildIDs returns the IDs of all guest guilds attached to this session
// (host excluded). O(guests) — the set is maintained by AddGuild/RemoveGuild.
func (s *Session) GuestGuildIDs() []snowflake.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]snowflake.ID, 0, len(s.guests))
	for id := range s.guests {
		ids = append(ids, id)
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
// O(guests-of-this-session) thanks to Session.GuestGuildIDs (vs the prior
// O(all-attached-guilds) scan of m.byGuild).
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
	for _, guildID := range s.GuestGuildIDs() {
		delete(m.byGuild, guildID)
	}
}
