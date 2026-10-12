package dave

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/disgoorg/godave"
)

// Instrument wraps a DAVE session factory so every frame decryption is counted
// in stats, keyed by the sending user, and so frames from users the backend has
// no decryptor for are passed through correctly (see countingSession.Decrypt).
//
// Every other method is the backend's own. Pass the result wherever the raw
// backend factory would go — the choice of backend stays a one-line decision at
// the call site.
func Instrument(inner godave.SessionCreateFunc, stats *Stats, opts ...Option) godave.SessionCreateFunc {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return func(logger *slog.Logger, selfUserID godave.UserID, callbacks godave.Callbacks) godave.Session {
		return &countingSession{
			Session:     inner(logger, selfUserID, callbacks),
			stats:       stats,
			botUserID:   string(selfUserID),
			selfUserID:  selfUserID,
			callbacks:   callbacks,
			stallFrames: int64(o.stallFrames),
			onStall:     o.onStall,
			known:       make(map[godave.UserID]struct{}),
		}
	}
}

// Option configures Instrument.
type Option func(*options)

type options struct {
	stallFrames int
	onStall     EncryptStallFunc
}

// EncryptStallFunc is told which bot's session has stopped encrypting, and the
// callbacks value disgo created that session with — the bot's voice.Conn. It
// runs on disgo's audio send goroutine and must not block.
type EncryptStallFunc func(selfUserID godave.UserID, conn godave.Callbacks)

// EncryptStallFrames is 10 s of outbound speech at 20 ms per frame.
const EncryptStallFrames = 500

// WithEncryptStall reports a session each time another frames consecutive
// outbound frames have failed to encrypt, so a repair that did not take is
// retried. A frame that encrypts resets the count.
//
// The failure this exists for: golibdave v0.3.0 copies the bot's own key
// ratchet from the MLS session when a transition executes, and if the group is
// not established at that moment the encryptor is left with none. Every frame
// then fails with "missing key ratchet" until the next membership change in
// the channel happens to repair it — observed lasting an hour in production.
func WithEncryptStall(frames int, notify EncryptStallFunc) Option {
	return func(o *options) {
		o.stallFrames = frames
		o.onStall = notify
	}
}

// EncryptStalls forwards stall reports to a handler installed after the voice
// clients are built, since the clients exist before whatever acts on a stall.
// Reports arriving before SetHandler are dropped.
type EncryptStalls struct {
	handler atomic.Pointer[EncryptStallFunc]
}

func NewEncryptStalls() *EncryptStalls { return &EncryptStalls{} }

func (s *EncryptStalls) SetHandler(f EncryptStallFunc) { s.handler.Store(&f) }

// Notify is an EncryptStallFunc.
func (s *EncryptStalls) Notify(selfUserID godave.UserID, conn godave.Callbacks) {
	if f := s.handler.Load(); f != nil {
		(*f)(selfUserID, conn)
	}
}

// Option returns the WithEncryptStall option that reports into s.
func (s *EncryptStalls) Option() Option {
	return WithEncryptStall(EncryptStallFrames, s.Notify)
}

// countingSession counts decrypt outcomes and mirrors the backend's set of
// known users.
//
// The mirror is exact for golibdave v0.3.0: its decryptors map is written only
// by AddUser and RemoveUser, both of which are interface methods that pass
// through here. Re-verify that when the backend is upgraded — if the backend
// gained a decryptor this wrapper does not know about, Decrypt would pass
// encrypted audio through as plaintext.
//
// UPSTREAM(golibdave v0.3.0): the mirror assumes only AddUser and RemoveUser
// ever write the backend's decryptor map.
type countingSession struct {
	godave.Session
	stats     *Stats
	botUserID string

	selfUserID   godave.UserID
	callbacks    godave.Callbacks
	stallFrames  int64
	onStall      EncryptStallFunc
	encryptFails atomic.Int64

	mu    sync.RWMutex
	known map[godave.UserID]struct{}
}

// Decrypt counts the outcome and handles the no-decryptor case itself.
//
// golibdave v0.3.0 answers an unknown user with passthrough, but its copy has
// its arguments swapped (golibdave.go:104, `copy(frame, decryptedFrame)`), so
// it returns a plausible length having written nothing: the caller reads back
// the untouched output buffer, i.e. a frame of silence, with no error. Handling
// the case here never reaches that branch.
//
// What the right answer is depends on whether the group is encrypted. While the
// session is in passthrough mode the frame is plain Opus and copying it through
// is correct. Once an epoch is active it is ciphertext, and disgo hands us
// UnknownUserID for any SSRC it has no SPEAKING op for yet — routine on join —
// so forwarding it would feed the mixer and relay garbage audio. Dropping
// returns a zero-length frame rather than an error, which disgo would turn into
// a log line per packet.
//
// UPSTREAM(golibdave v0.3.0): the unknown-user passthrough copies its arguments
// the wrong way round (golibdave.go:104), returning a frame of silence.
func (s *countingSession) Decrypt(userID godave.UserID, frame []byte, decryptedFrame []byte) (int, error) {
	if !s.knows(userID) {
		if s.Session.Ready() {
			s.stats.RecordDropped(s.botUserID, string(userID))
			return 0, nil
		}

		s.stats.RecordPassthrough(s.botUserID, string(userID))
		// cap, not len: disgo sizes this buffer with slices.Grow, which raises
		// capacity and leaves length at its original 512.
		return copy(decryptedFrame[:cap(decryptedFrame)], frame), nil
	}

	n, err := s.Session.Decrypt(userID, frame, decryptedFrame)
	s.stats.Record(s.botUserID, string(userID), err)
	return n, err
}

// Encrypt reports the session through onStall once outbound encryption has
// been failing long enough to mean the bot is silent; see WithEncryptStall.
func (s *countingSession) Encrypt(ssrc uint32, frame []byte, encryptedFrame []byte) (int, error) {
	n, err := s.Session.Encrypt(ssrc, frame, encryptedFrame)
	if err == nil {
		s.encryptFails.Store(0)
		return n, nil
	}
	if s.onStall != nil && s.stallFrames > 0 && s.encryptFails.Add(1)%s.stallFrames == 0 {
		s.onStall(s.selfUserID, s.callbacks)
	}
	return n, err
}

// AddUser and RemoveUser keep the mirror on the conservative side of the
// backend: a user is in known only while the backend certainly has a decryptor
// for them. AddUser therefore records the user after the backend call — libdave
// sets up the key ratchet in there, and a frame arriving meanwhile must take
// this wrapper's path rather than the backend's broken one — and RemoveUser
// drops them before it. The cost either way is a frame handled here that the
// backend could have decrypted; the alternative is a frame delegated to a
// backend that cannot, counted as a success and silently replaced with silence.
func (s *countingSession) AddUser(userID godave.UserID) {
	s.Session.AddUser(userID)

	if s.remember(userID) {
		s.stats.Keep(s.botUserID, string(userID))
	}
}

func (s *countingSession) RemoveUser(userID godave.UserID) {
	if s.forget(userID) {
		s.stats.Retire(s.botUserID, string(userID))
	}

	s.Session.RemoveUser(userID)
}

func (s *countingSession) knows(userID godave.UserID) bool {
	s.mu.RLock()
	_, ok := s.known[userID]
	s.mu.RUnlock()
	return ok
}

// remember records userID and reports whether it was new, so the stats
// refcount moves once per session even if the gateway repeats the add.
func (s *countingSession) remember(userID godave.UserID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.known[userID]; ok {
		return false
	}
	s.known[userID] = struct{}{}
	return true
}

// forget drops userID and reports whether this session was still holding it.
func (s *countingSession) forget(userID godave.UserID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.known[userID]; !ok {
		return false
	}
	delete(s.known, userID)
	return true
}
