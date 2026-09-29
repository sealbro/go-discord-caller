package dave

import (
	"log/slog"
	"sync"

	"github.com/disgoorg/godave"
)

// Instrument wraps a DAVE session factory so every frame decryption is counted
// in stats, keyed by the sending user, and so frames from users the backend has
// no decryptor for are passed through correctly (see countingSession.Decrypt).
//
// Every other method is the backend's own. Pass the result wherever the raw
// backend factory would go — the choice of backend stays a one-line decision at
// the call site.
func Instrument(inner godave.SessionCreateFunc, stats *Stats) godave.SessionCreateFunc {
	return func(logger *slog.Logger, selfUserID godave.UserID, callbacks godave.Callbacks) godave.Session {
		return &countingSession{
			Session:   inner(logger, selfUserID, callbacks),
			stats:     stats,
			botUserID: string(selfUserID),
			known:     make(map[godave.UserID]struct{}),
		}
	}
}

// countingSession counts decrypt outcomes and mirrors the backend's set of
// known users.
//
// The mirror is exact for golibdave v0.3.0: its decryptors map is written only
// by AddUser and RemoveUser, both of which are interface methods that pass
// through here. Re-verify that when the backend is upgraded — if the backend
// gained a decryptor this wrapper does not know about, Decrypt would pass
// encrypted audio through as plaintext.
type countingSession struct {
	godave.Session
	stats     *Stats
	botUserID string

	mu    sync.RWMutex
	known map[godave.UserID]struct{}
}

// Decrypt counts the outcome and handles the no-decryptor case itself.
//
// golibdave v0.3.0 answers an unknown user with passthrough, but its copy has
// its arguments swapped (golibdave.go:104, `copy(frame, decryptedFrame)`), so
// it returns a plausible length having written nothing: the caller reads back
// the untouched output buffer, i.e. a frame of silence, with no error. Doing
// the copy here keeps those frames intact and never reaches that branch.
func (s *countingSession) Decrypt(userID godave.UserID, frame []byte, decryptedFrame []byte) (int, error) {
	if !s.knows(userID) {
		s.stats.RecordPassthrough(s.botUserID, string(userID))
		// cap, not len: disgo sizes this buffer with slices.Grow, which raises
		// capacity and leaves length at its original 512.
		return copy(decryptedFrame[:cap(decryptedFrame)], frame), nil
	}

	n, err := s.Session.Decrypt(userID, frame, decryptedFrame)
	s.stats.Record(s.botUserID, string(userID), err)
	return n, err
}

func (s *countingSession) AddUser(userID godave.UserID) {
	s.mu.Lock()
	s.known[userID] = struct{}{}
	s.mu.Unlock()

	s.stats.Keep(s.botUserID, string(userID))
	s.Session.AddUser(userID)
}

func (s *countingSession) RemoveUser(userID godave.UserID) {
	s.mu.Lock()
	delete(s.known, userID)
	s.mu.Unlock()

	s.stats.Retire(s.botUserID, string(userID))
	s.Session.RemoveUser(userID)
}

func (s *countingSession) knows(userID godave.UserID) bool {
	s.mu.RLock()
	_, ok := s.known[userID]
	s.mu.RUnlock()
	return ok
}
