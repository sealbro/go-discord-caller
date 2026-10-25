package pool

import (
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
)

// newPacedAudioReceiver builds disgo's audio receiver on a conn whose DAVE
// session waits one frame before reporting not ready.
//
// disgo's receive loop has no pacing of its own: it blocks only in ReadPacket,
// and skips that while the conn's DAVE session is not ready:
//
//	for { select { case <-ctx.Done(): ...; default: s.receive() } }
//
//	func (s *defaultAudioReceiver) receive() {
//		if !s.conn.DAVE().Ready() { return }
//		packet, err := s.conn.UDP().ReadPacket()
//
// golibdave reports not ready while the bot's own encryptor is in passthrough
// or has lost its key ratchet, which can last for hours, and each such conn
// then spins a core. In production that pinned a one-vCPU host and pushed
// every bot's gateway heartbeat from ~110 ms to ~545 ms.
//
// Only the receiver sees the paced session: the sender paces its own loop.
//
// UPSTREAM(disgo 5bf95eb): the audio receiver busy-loops while DAVE is not ready.
func newPacedAudioReceiver(logger *slog.Logger, receiver voice.OpusFrameReceiver, conn voice.Conn) voice.AudioReceiver {
	return voice.NewAudioReceiver(logger, receiver, pacedDaveConn{Conn: conn, dave: pacedDaveSession{conn.DAVE()}})
}

type pacedDaveConn struct {
	voice.Conn
	dave godave.Session
}

func (c pacedDaveConn) DAVE() godave.Session { return c.dave }

type pacedDaveSession struct{ godave.Session }

func (s pacedDaveSession) Ready() bool {
	if s.Session.Ready() {
		return true
	}
	time.Sleep(voice.OpusFrameSizeMs * time.Millisecond)
	return false
}
