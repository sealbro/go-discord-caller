package pool

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
)

// newPacedAudioReceiver builds disgo's audio receiver on a conn whose DAVE
// session waits one frame before reporting not ready, and registers it so the
// conn's removal can stop it.
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
// connImpl.Close also leaves the receiver running, like the sender. It exits by
// itself once ReadPacket fails on the closed socket, but one that is not ready
// never reads, and golibdave's Close does not change readiness.
//
// UPSTREAM(disgo 5bf95eb): the audio receiver busy-loops while DAVE is not
// ready, and Conn.Close leaves it running.
func newPacedAudioReceiver(logger *slog.Logger, receiver voice.OpusFrameReceiver, conn voice.Conn) voice.AudioReceiver {
	paced := &pacedDaveConn{Conn: conn, dave: pacedDaveSession{conn.DAVE()}, started: make(chan struct{})}
	r := &safeAudioReceiver{
		AudioReceiver: voice.NewAudioReceiver(logger, &onceClosingReceiver{OpusFrameReceiver: receiver}, paced),
		started:       paced.started,
	}
	defaultReceivers.mu.Lock()
	defaultReceivers.receivers[conn] = r
	defaultReceivers.mu.Unlock()
	return r
}

// pacedDaveConn also closes started on the first DAVE() call, which the
// receive loop makes after Open has set the cancelFunc that Close reads; see
// startSignalConn.
type pacedDaveConn struct {
	voice.Conn
	dave    godave.Session
	once    sync.Once
	started chan struct{}
}

func (c *pacedDaveConn) DAVE() godave.Session {
	c.once.Do(func() { close(c.started) })
	return c.dave
}

type pacedDaveSession struct{ godave.Session }

func (s pacedDaveSession) Ready() bool {
	if s.Session.Ready() {
		return true
	}
	time.Sleep(voice.OpusFrameSizeMs * time.Millisecond)
	return false
}

// onceClosingReceiver makes the opus receiver's Close run once. disgo's receive
// goroutine closes the receiver itself when ReadPacket finds the socket closed,
// which can race the registry closing it, and opus.VoiceReceiver.Close is not
// safe to call concurrently.
type onceClosingReceiver struct {
	voice.OpusFrameReceiver
	once sync.Once
}

func (r *onceClosingReceiver) Close() {
	r.once.Do(r.OpusFrameReceiver.Close)
}

// safeAudioReceiver is safeAudioSender for the receive side: disgo's
// defaultAudioReceiver.Close has the same unsynchronised cancelFunc read.
type safeAudioReceiver struct {
	voice.AudioReceiver
	once    sync.Once
	started <-chan struct{}
}

func (r *safeAudioReceiver) Close() {
	r.once.Do(func() {
		select {
		case <-r.started:
		case <-time.After(senderStartTimeout):
			slog.Warn("voice: audio receiver never started; closing anyway")
		}
		defer func() {
			if p := recover(); p != nil {
				slog.Warn("voice: recovered panic closing an audio receiver (disgo defaultAudioReceiver.Close nil cancelFunc)",
					slog.Any("panic", p))
			}
		}()
		r.AudioReceiver.Close()
	})
}

// audioReceivers tracks the receiver behind each conn. disgo's
// SetOpusFrameReceiver closes the previous receiver before creating the next,
// so overwriting an entry never orphans a running one.
type audioReceivers struct {
	mu        sync.Mutex
	receivers map[voice.Conn]*safeAudioReceiver
}

func (a *audioReceivers) close(conn voice.Conn) {
	a.mu.Lock()
	r, ok := a.receivers[conn]
	delete(a.receivers, conn)
	a.mu.Unlock()
	if ok {
		r.Close()
	}
}

var defaultReceivers = &audioReceivers{receivers: make(map[voice.Conn]*safeAudioReceiver)}
