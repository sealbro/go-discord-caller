package dave

import (
	"testing"

	"github.com/disgoorg/godave"
)

// decryptUnknown sends one plain frame from a user with no decryptor and
// returns what came out.
func decryptUnknown(t *testing.T, session godave.Session) string {
	t.Helper()
	frame := []byte("opus-audio")
	out := make([]byte, 0, 512)
	n, err := session.Decrypt(godave.UserID(UnknownUserID), frame, out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return string(out[:cap(out)][:n])
}

// golibdave reports a session on the disabled protocol version as ready, since
// E2EE is never coming, yet its frames are plain Opus. Treating that ready as
// "encrypted" drops every frame that arrives before its sender's SPEAKING op.
func TestUnknownUserFrameIsCopiedWhileProtocolDisabled(t *testing.T) {
	session, _ := newTestSession(t, NewStats(), "bot-1", nil)
	session.OnSelectProtocolAck(0)

	if got := decryptUnknown(t, session); got != "opus-audio" {
		t.Errorf("output = %q, want the plain frame copied through", got)
	}
}

func TestUnknownUserFrameFollowsDowngradeTransition(t *testing.T) {
	session, backend := newTestSession(t, NewStats(), "bot-1", nil)
	session.OnSelectProtocolAck(1)
	backend.ready = true

	session.OnDavePrepareTransition(7, 0)
	if got := decryptUnknown(t, session); got != "" {
		t.Fatalf("output = %q before the downgrade executed, want the ciphertext dropped", got)
	}

	session.OnDaveExecuteTransition(7)
	backend.ready = false
	if got := decryptUnknown(t, session); got != "opus-audio" {
		t.Errorf("output = %q after the downgrade, want the plain frame copied through", got)
	}
}

func TestUnknownUserFrameIsDroppedAgainAfterReenable(t *testing.T) {
	session, backend := newTestSession(t, NewStats(), "bot-1", nil)
	session.OnSelectProtocolAck(0)

	session.OnDaveMLSWelcome(9, nil)
	session.OnDaveExecuteTransition(9)
	backend.ready = true

	if got := decryptUnknown(t, session); got != "" {
		t.Errorf("output = %q once E2EE is back, want the ciphertext dropped", got)
	}
}

// disgo hands Decrypt a buffer whose length stays at 512 while its capacity is
// grown to fit the frame. A frame over 512 bytes — Opus at 256 kbps and up, on
// boosted servers — must still come through whole, with the copy left to
// golibdave.
func TestUnknownUserFrameIsPassedThroughWhole(t *testing.T) {
	session, backend := newTestSession(t, NewStats(), "bot-1", nil)

	frame := make([]byte, 900)
	for i := range frame {
		frame[i] = byte(i)
	}
	out := make([]byte, 512, len(frame))

	n, err := session.Decrypt(godave.UserID(UnknownUserID), frame, out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(frame) || string(out[:cap(out)][:n]) != string(frame) {
		t.Errorf("got %d bytes, want the whole %d-byte frame", n, len(frame))
	}
	if backend.decryptCalls() != 1 {
		t.Errorf("backend Decrypt was called %d times, want the passthrough left to golibdave", backend.decryptCalls())
	}
}
