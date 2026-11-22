package ally

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// Code is the unique 8-character code that identifies an ally session.
type Code = string

// RelaySink receives every packet another guild broadcasts into a session,
// with the guild it came from. It takes ownership of pkt and must return it
// via opus.PutEncodedFrame. It runs on the broadcaster's goroutine with the
// session lock held, so it must not block or call back into the session.
type RelaySink func(srcGuildID snowflake.ID, pkt []byte)

// Session is an active cross-guild audio relay owned by one host guild.
// Every capturing guild broadcasts its own stream via BroadcastFromGuild, and
// nothing merges them on the way: a guild in a session of three is sent two.
// A guild therefore attaches in one of two ways. AddReceiver takes each peer's
// packets tagged with their source, for a guild that mixes them. AddListener
// takes speaker output queues, which carry a single stream each and so are
// only ever sent the host's.
type Session struct {
	Code        Code
	HostGuildID snowflake.ID
	HostMode    guild.RaidMode

	done      chan struct{}
	closeOnce sync.Once

	mu        sync.RWMutex
	listeners map[snowflake.ID][]chan<- []byte // guildID → speaker chOut channels
	receivers map[snowflake.ID]RelaySink
	guests    map[snowflake.ID]struct{} // guest guild IDs (host not included)
	capturing map[snowflake.ID]struct{} // guilds that may broadcast into the session
	observers map[snowflake.ID]func()   // guildID → membership-change callback
}

func newSession(code Code, hostGuildID snowflake.ID, mode guild.RaidMode) *Session {
	return &Session{
		Code:        code,
		HostGuildID: hostGuildID,
		HostMode:    mode,
		done:        make(chan struct{}),
		listeners:   make(map[snowflake.ID][]chan<- []byte),
		receivers:   make(map[snowflake.ID]RelaySink),
		guests:      make(map[snowflake.ID]struct{}),
		capturing:   make(map[snowflake.ID]struct{}),
		observers:   make(map[snowflake.ID]func()),
	}
}

// broadcast sends pkt from srcGuildID to every other guild's receiver and,
// when the source is the host, to every other guild's listener queues.
// Must be called with s.mu held for reading.
// Takes ownership of pkt: each channel and sink receives its own copy so that
// they can independently return their buffer to the pool via
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
// Both owners follow this order: the host pipelines' session cleanup
// (`pipeline.hostRelayInputs.close`) and the guest JoinSession defer in
// `manager/voice_raid.go`. If you add a new caller that
// owns channels registered via AddListener, or a sink that sends on a channel
// it closes, preserve this ordering.
func (s *Session) broadcast(pkt []byte, srcGuildID snowflake.ID) {
	if srcGuildID == s.HostGuildID {
		for guildID, chs := range s.listeners {
			if guildID == srcGuildID {
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
	}
	for guildID, sink := range s.receivers {
		if guildID == srcGuildID {
			continue
		}
		sink(srcGuildID, opus.CopyOpusFrame(pkt))
	}
	opus.PutEncodedFrame(pkt)
}

// BroadcastFromGuild fans a packet out to every guild except srcGuildID, so a
// guild's speakers never play back what its own users said.
// Non-blocking: full channels drop the frame.
func (s *Session) BroadcastFromGuild(srcGuildID snowflake.ID, pkt []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.broadcast(pkt, srcGuildID)
}

// AddListener attaches a guild that plays the host's stream straight into its
// speaker output channels.
func (s *Session) AddListener(guildID snowflake.ID, chs []chan<- []byte) {
	s.attach(guildID, func() { s.listeners[guildID] = chs })
}

// AddReceiver attaches a guild that takes every peer's stream through sink.
func (s *Session) AddReceiver(guildID snowflake.ID, sink RelaySink) {
	s.attach(guildID, func() { s.receivers[guildID] = sink })
}

func (s *Session) attach(guildID snowflake.ID, register func()) {
	s.mu.Lock()
	register()
	if guildID != s.HostGuildID {
		s.guests[guildID] = struct{}{}
	}
	fns := s.observersLocked()
	s.mu.Unlock()
	notify(fns)
}

// Attached reports whether guildID is the host or a guest currently attached,
// i.e. whether it can still broadcast into the session.
func (s *Session) Attached(guildID snowflake.ID) bool {
	if guildID == s.HostGuildID {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.guests[guildID]
	return ok
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
	delete(s.listeners, guildID)
	delete(s.receivers, guildID)
	delete(s.guests, guildID)
	delete(s.capturing, guildID)
	delete(s.observers, guildID)
	fns := s.observersLocked()
	s.mu.Unlock()
	notify(fns)
}

// GuestGuildIDs returns the IDs of all guest guilds attached to this session
// (host excluded). O(guests) — the set is maintained on attach and RemoveGuild.
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
	s.closeOnce.Do(func() { close(s.done) })
}

// codeLength is the length of a relay code: eight characters of the RFC 4648
// base32 alphabet, which is what the store generates.
const codeLength = 8

// NormalizeCode trims and upper-cases a relay code typed by a person and
// reports whether the result has the shape of one the store could have issued.
func NormalizeCode(raw string) (Code, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != codeLength {
		return "", false
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return "", false
		}
	}
	return code, true
}

const (
	joinMaxFailures = 5
	joinLockout     = 10 * time.Minute
)

// ErrJoinLockedOut is returned by Join once a guild has tried too many wrong
// codes. While it holds, even a correct code is refused, so the lockout cannot
// be used to tell a right guess from a wrong one.
var ErrJoinLockedOut = errors.New("too many wrong relay codes: try again later")

// ErrNoSession is returned by Join when no running session has the code: it
// is wrong, or the host has not started its raid yet.
var ErrNoSession = errors.New("no relay session matches this code")

// Manager is the global registry of active ally sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[Code]*Session         // code → session
	byGuild  map[snowflake.ID]*Session // guildID (host or guest) → session
	failures map[snowflake.ID][]time.Time
	now      func() time.Time
}

func NewManager() *Manager {
	return &Manager{
		sessions: make(map[Code]*Session),
		byGuild:  make(map[snowflake.ID]*Session),
		failures: make(map[snowflake.ID][]time.Time),
		now:      time.Now,
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

// ErrCodeInUse is returned by Open when the code or the host guild is already
// registered to a session of another guild.
var ErrCodeInUse = errors.New("relay code or host guild already belongs to another session")

// Open is Create with guards: it refuses a code held by another guild's session
// and a host guild that is attached to another guild's session as a guest. A
// stale session of the same host is closed and replaced, so its guests are
// released instead of being orphaned by the overwrite.
func (m *Manager) Open(code Code, hostGuildID snowflake.ID, mode guild.RaidMode) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.sessions[code]; ok && held.HostGuildID != hostGuildID {
		return nil, ErrCodeInUse
	}
	if held, ok := m.byGuild[hostGuildID]; ok && held.HostGuildID != hostGuildID {
		return nil, ErrCodeInUse
	}
	if stale, ok := m.byGuild[hostGuildID]; ok {
		m.removeHostedLocked(stale)
	}
	if stale, ok := m.sessions[code]; ok {
		m.removeHostedLocked(stale)
	}
	s := newSession(code, hostGuildID, mode)
	m.sessions[s.Code] = s
	m.byGuild[hostGuildID] = s
	return s, nil
}

// Join attaches guildID to the session identified by code.
func (m *Manager) Join(code Code, guildID snowflake.ID) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockedOutLocked(guildID) {
		return nil, ErrJoinLockedOut
	}
	s, ok := m.sessions[code]
	if !ok {
		m.recordFailureLocked(guildID)
		return nil, ErrNoSession
	}
	if s.HostGuildID == guildID {
		return nil, fmt.Errorf("cannot join your own session")
	}
	if _, already := m.byGuild[guildID]; already {
		return nil, fmt.Errorf("guild already has an active session")
	}
	delete(m.failures, guildID)
	m.byGuild[guildID] = s
	return s, nil
}

// recentFailuresLocked returns guildID's wrong-code attempts still inside the
// lockout window, dropping the older ones. Caller must hold m.mu.
func (m *Manager) recentFailuresLocked(guildID snowflake.ID) []time.Time {
	cutoff := m.now().Add(-joinLockout)
	recent := m.failures[guildID][:0]
	for _, at := range m.failures[guildID] {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	if len(recent) == 0 {
		delete(m.failures, guildID)
		return nil
	}
	m.failures[guildID] = recent
	return recent
}

func (m *Manager) lockedOutLocked(guildID snowflake.ID) bool {
	return len(m.recentFailuresLocked(guildID)) >= joinMaxFailures
}

func (m *Manager) recordFailureLocked(guildID snowflake.ID) {
	recent := m.recentFailuresLocked(guildID)
	m.failures[guildID] = append(recent, m.now())
}

// GetByGuild returns the session the guild belongs to (as host or guest).
func (m *Manager) GetByGuild(guildID snowflake.ID) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byGuild[guildID]
	return s, ok
}

// RemoveGuest detaches a guest guild from session s without affecting other
// participants. A guild registered to a different session (a newer join after
// s ended) is left alone.
func (m *Manager) RemoveGuest(guildID snowflake.ID, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byGuild[guildID] == s {
		delete(m.byGuild, guildID)
	}
}

// RemoveHost closes the session hosted by hostGuildID and removes all
// participants from the registry. A guild attached as a guest is not a host
// and is left alone.
// O(guests-of-this-session) thanks to Session.GuestGuildIDs (vs the prior
// O(all-attached-guilds) scan of m.byGuild).
func (m *Manager) RemoveHost(hostGuildID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byGuild[hostGuildID]
	if !ok || s.HostGuildID != hostGuildID {
		return
	}
	m.removeHostedLocked(s)
}

// Remove closes s and removes it and its guests from the registry, unless s is
// no longer the session registered for its host.
func (m *Manager) Remove(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byGuild[s.HostGuildID] != s {
		return
	}
	m.removeHostedLocked(s)
}

func (m *Manager) removeHostedLocked(s *Session) {
	s.close()
	if m.sessions[s.Code] == s {
		delete(m.sessions, s.Code)
	}
	if m.byGuild[s.HostGuildID] == s {
		delete(m.byGuild, s.HostGuildID)
	}
	for _, guildID := range s.GuestGuildIDs() {
		if m.byGuild[guildID] == s {
			delete(m.byGuild, guildID)
		}
	}
}
