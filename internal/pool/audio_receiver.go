package pool

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
)

// newOwnedAudioReceiver builds disgo's audio receiver and registers it so the
// conn's removal can stop it.
//
// connImpl.Close leaves the receiver running, like the sender. It exits by
// itself once ReadPacket fails on the closed socket, but one whose DAVE session
// is not ready never reads, and golibdave's Close does not change readiness.
//
// UPSTREAM(disgo v0.20.0): Conn.Close leaves the audio receiver running.
func newOwnedAudioReceiver(logger *slog.Logger, receiver voice.OpusFrameReceiver, conn voice.Conn) voice.AudioReceiver {
	watched := &startSignalConn{Conn: conn, dave: conn.DAVE(), started: make(chan struct{})}
	r := &safeAudioReceiver{
		AudioReceiver: voice.NewAudioReceiver(logger, &onceClosingReceiver{OpusFrameReceiver: receiver}, watched),
		started:       watched.started,
	}
	defaultReceivers.mu.Lock()
	defaultReceivers.receivers[conn] = r
	defaultReceivers.mu.Unlock()
	return r
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
