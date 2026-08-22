package pool

import (
	"log/slog"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
)

// safeUDPConn wraps disgo's voice UDP connection so that Close can never panic.
//
// disgo v0.19.3 dereferences the socket unconditionally:
//
//	func (u *udpConnImpl) Close() error {
//		u.connMu.Lock()
//		defer u.connMu.Unlock()
//		return u.conn.Close() // net.Conn, nil until Open() completes
//	}
//
// The field stays nil until the UDP handshake finishes, so closing a voice
// connection whose Open timed out panics inside the library. Two paths reach
// it, and an unrecovered panic on either one kills the whole bot process:
//
//  1. our own teardown — manager.joinSpeakers drops a half-open conn via
//     GuildVoice.Leave after a join failure, and every session cleanup path
//     closes conns that may never have opened;
//  2. disgo's gateway goroutine — connImpl.HandleVoiceStateUpdate closes the
//     UDP conn when Discord reports the bot was disconnected.
//
// Path 2 runs inside the library, so no amount of recover() on our side can
// contain it. Wrapping the UDPConn itself fixes both: the panic is contained
// at its source, in the one method that raises it.
//
// Join timeouts are routine (gateway hiccup, Discord voice rate limit), which
// makes this a live crash risk rather than a test-only annoyance.
//
// Everything except Close is promoted from the embedded interface, so this
// stays a pure pass-through of disgo's own implementation.
type safeUDPConn struct {
	voice.UDPConn
}

// Close closes the underlying UDP conn, converting the library's nil-deref
// panic into a no-op. Closing a socket that was never opened has nothing to
// release, so nil is the correct result. Real errors are returned unchanged.
func (c *safeUDPConn) Close() (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("voice: recovered panic closing an unopened UDP conn (disgo udpConnImpl.Close nil deref)",
				slog.Any("panic", r))
			err = nil
		}
	}()
	return c.UDPConn.Close()
}

// NewSafeUDPConn is a voice.UDPConnCreateFunc that builds disgo's standard UDP
// conn behind the safeUDPConn guard.
func NewSafeUDPConn(daveSession godave.Session, ssrcLookup voice.SsrcLookupFunc, opts ...voice.UDPConnConfigOpt) voice.UDPConn {
	return &safeUDPConn{UDPConn: voice.NewUDPConn(daveSession, ssrcLookup, opts...)}
}

// SafeUDPConnOpt installs NewSafeUDPConn on a bot's voice manager. Every client
// this process creates — owner, speaker pool, and the integration harness bots —
// must pass it, since any of them can be told to leave a channel it never
// finished joining.
//
// voice.WithConnConfigOpts appends, so this composes with
// voice.WithDaveSessionCreateFunc (itself a ConnConfigOpt) rather than
// replacing it.
func SafeUDPConnOpt() voice.ManagerConfigOpt {
	return voice.WithConnConfigOpts(voice.WithUDPConnCreateFunc(NewSafeUDPConn))
}
