package pool

import (
	"testing"

	"github.com/disgoorg/disgo/voice"
)

// Closing a voice UDP conn whose Open never completed — the state left behind
// by a join timeout — must be a harmless no-op rather than a process-killing
// panic.
func TestSafeUDPConn_CloseBeforeOpen(t *testing.T) {
	t.Parallel()
	conn := NewSafeUDPConn(nil, nil)
	if err := conn.Close(); err != nil {
		t.Errorf("Close on an unopened conn: want nil error, got %v", err)
	}
	// Idempotent: session teardown can close the same conn more than once.
	if err := conn.Close(); err != nil {
		t.Errorf("second Close: want nil error, got %v", err)
	}
}

// Tripwire for the upstream bug this wrapper exists to contain. When disgo
// starts guarding udpConnImpl.Close against a nil socket, this test fails —
// that is the signal to delete safeUDPConn, SafeUDPConnOpt, its call sites in
// bot.NewOwnerClient / newPoolClient / the integration harness, and this file.
func TestDisgoUDPConnStillPanicsOnCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("disgo's udpConnImpl.Close no longer panics on an unopened conn — " +
				"the safeUDPConn workaround is obsolete and should be removed")
		}
	}()
	_ = voice.NewUDPConn(nil, nil).Close()
}
