package dave

import (
	"log/slog"

	"github.com/disgoorg/godave"
)

// Instrument wraps a DAVE session factory so every frame decryption is counted
// in stats, keyed by the sending user.
//
// The wrapper is transparent: every other method is the backend's own, and
// Decrypt returns exactly what the backend returned. Pass the result wherever
// the raw backend factory would go — the choice of backend stays a one-line
// decision at the call site.
func Instrument(inner godave.SessionCreateFunc, stats *Stats) godave.SessionCreateFunc {
	return func(logger *slog.Logger, selfUserID godave.UserID, callbacks godave.Callbacks) godave.Session {
		return &countingSession{
			Session:   inner(logger, selfUserID, callbacks),
			stats:     stats,
			botUserID: string(selfUserID),
		}
	}
}

type countingSession struct {
	godave.Session
	stats     *Stats
	botUserID string
}

func (s *countingSession) Decrypt(userID godave.UserID, frame []byte, decryptedFrame []byte) (int, error) {
	n, err := s.Session.Decrypt(userID, frame, decryptedFrame)
	s.stats.Record(s.botUserID, string(userID), err)
	return n, err
}
