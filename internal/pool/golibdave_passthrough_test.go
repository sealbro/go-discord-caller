package pool

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/disgoorg/godave/golibdave"
)

type nopDaveCallbacks struct{}

func (nopDaveCallbacks) SendMLSKeyPackage([]byte) error        { return nil }
func (nopDaveCallbacks) SendMLSCommitWelcome([]byte) error     { return nil }
func (nopDaveCallbacks) SendReadyForTransition(uint16) error   { return nil }
func (nopDaveCallbacks) SendInvalidCommitWelcome(uint16) error { return nil }

// TestGolibdavePassthroughFillsCapacity guards the golibdave version. disgo
// grows the decrypt buffer's capacity to fit the frame but leaves its length at
// 512, and dave.countingSession hands that buffer to golibdave unchanged.
// golibdave before b817bbd (godave PR #15) copies a passthrough frame into the
// length, cutting anything over 512 bytes short without an error. When this
// fails, go.mod moved to a godave without that fix.
func TestGolibdavePassthroughFillsCapacity(t *testing.T) {
	s := golibdave.NewSession(slog.New(slog.DiscardHandler), "1", nopDaveCallbacks{})

	frame := bytes.Repeat([]byte{0xAB}, 900)
	out := make([]byte, 512, len(frame))

	n, err := s.Decrypt("2", frame, out)
	if err != nil || n != len(frame) || !bytes.Equal(out[:n], frame) {
		t.Fatalf("passthrough returned %d of %d bytes (err %v): frames over 512 bytes are cut short", n, len(frame), err)
	}
}
