package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/dave"
)

// ratchetlessSession stands in for a golibdave session whose encryptor has no
// key ratchet: golibdave reports it not ready, and every Encrypt would fail.
type ratchetlessSession struct{ godave.Session }

func (ratchetlessSession) Ready() bool                   { return false }
func (ratchetlessSession) SetChannelID(godave.ChannelID) {}
func (ratchetlessSession) Close() error                  { return nil }
func (ratchetlessSession) Encrypt(uint32, []byte, []byte) (int, error) {
	return 0, errors.New("missing key ratchet")
}

// speakingProvider always has a frame to send, as a speaker relaying a caller does.
type speakingProvider struct{}

func (speakingProvider) ProvideOpusFrame() ([]byte, error) { return []byte{0xF8, 0xFF, 0xFE}, nil }
func (speakingProvider) Close()                            {}

// Production, 2026-10-10/11: a speaker whose encryptor lost its key ratchet was
// silent for up to an hour, and the encrypt-stall rejoin exists to end that
// within ~10 s of speech. disgo's sender now skips a tick while the DAVE
// session is not ready — which is exactly what a missing key ratchet reports —
// so the frame is never encrypted, never fails, and the stall is never seen.
func TestEncryptStallReportedWhileDaveNotReady(t *testing.T) {
	t.Parallel()

	const stallFrames = 5

	var stalls atomic.Int64
	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		snowflake.ID(1529403555667251310),
		voice.WithDaveSessionCreateFunc(dave.Instrument(
			func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return ratchetlessSession{} },
			dave.NewStats(),
			dave.WithEncryptStall(stallFrames, func(godave.UserID, godave.Callbacks) { stalls.Add(1) }),
		)),
		SafeAudioSenderOpt(),
		VoiceConnOpt(),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	conn := vm.CreateConn(snowflake.ID(1430511050704289835))
	conn.SetOpusFrameProvider(speakingProvider{})
	t.Cleanup(func() { defaultSenders.Close(conn) })

	// stallFrames ticks are 100 ms; a second is ten times that.
	deadline := time.Now().Add(time.Second)
	for stalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a speaker with audio to send and no key ratchet went %v without an encrypt stall "+
				"being reported (threshold %d frames): it stays silent and is never rejoined", time.Second, stallFrames)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
