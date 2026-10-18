package pool

import (
	"slices"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// voiceReadyWatchers routes voice gateway Ready events to whoever watches that
// conn. disgo takes one event handler per voice manager rather than per conn,
// and hands it the gateway instead of the conn; each conn builds its gateway
// once, so the gateway identifies the conn.
type voiceReadyWatchers struct {
	mu       sync.Mutex
	handlers map[voice.Gateway]func()
}

func newVoiceReadyWatchers() *voiceReadyWatchers {
	return &voiceReadyWatchers{handlers: make(map[voice.Gateway]func())}
}

func (w *voiceReadyWatchers) handle(gw voice.Gateway, op voice.Opcode, _ int, _ voice.GatewayMessageData) {
	if op != voice.OpcodeReady {
		return
	}
	w.mu.Lock()
	f := w.handlers[gw]
	w.mu.Unlock()
	if f != nil {
		f()
	}
}

func (w *voiceReadyWatchers) watch(conn voice.Conn, f func()) {
	w.mu.Lock()
	w.handlers[conn.Gateway()] = f
	w.mu.Unlock()
}

func (w *voiceReadyWatchers) forget(conn voice.Conn) {
	w.mu.Lock()
	delete(w.handlers, conn.Gateway())
	w.mu.Unlock()
}

var defaultReadyWatchers = newVoiceReadyWatchers()

// newConn builds disgo's conn with the Ready dispatcher installed, and releases
// the conn's ready watch and audio sender when disgo removes it. disgo removes
// a conn it closes itself, such as on a voice-server disconnect, and a later
// Leave then finds nothing to release. A reopen keeps the conn, and both.
func newConn(guildID, userID snowflake.ID, stateUpdate voice.StateUpdateFunc, removeConn func(), opts ...voice.ConnConfigOpt) voice.Conn {
	var conn voice.Conn
	conn = voice.NewConn(guildID, userID, stateUpdate, func() {
		removeConn()
		defaultReadyWatchers.forget(conn)
		defaultSenders.Close(conn)
	}, append(slices.Clip(opts), voice.WithConnEventHandlerFunc(defaultReadyWatchers.handle))...)
	return conn
}

// VoiceReadyOpt installs the process-wide Ready dispatcher on a bot's voice
// manager, along with the cleanup that stops a removed conn's audio sender.
// Every client must pass it, or OnVoiceReady never fires for its conns and
// SafeAudioSenderOpt's senders are never stopped.
func VoiceReadyOpt() voice.ManagerConfigOpt {
	return voice.WithConnCreateFunc(newConn)
}

// OnVoiceReady runs f on every voice gateway Ready of conn, replacing any
// earlier watch on it, until the conn is closed. f runs inline on
// the gateway's listen goroutine, so it must not block.
func OnVoiceReady(conn voice.Conn, f func()) {
	defaultReadyWatchers.watch(conn, f)
}
