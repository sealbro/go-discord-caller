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
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// ratchetlessSession stands in for a golibdave session whose encryptor has no
// key ratchet: golibdave reports it not ready, and every Encrypt would fail.
type ratchetlessSession struct{ godave.Session }

func (ratchetlessSession) Ready() bool                   { return false }
func (ratchetlessSession) SetChannelID(godave.ChannelID) {}
func (ratchetlessSession) Close() error                  { return nil }
func (ratchetlessSession) AddUser(godave.UserID)         {}
func (ratchetlessSession) Encrypt(uint32, []byte, []byte) (int, error) {
	return 0, errors.New("missing key ratchet")
}

// speakingCaller feeds ch a frame every 20 ms, as a speaker relaying a caller
// is fed, dropping frames while ch is full as the fanout does.
func speakingCaller(t *testing.T, ch chan<- []byte) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		tick := time.NewTicker(voice.OpusFrameSizeMs * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				select {
				case ch <- []byte{0xF8, 0xFF, 0xFE}:
				default:
				}
			}
		}
	}()
}

// Production, 2026-10-10/11: a speaker whose encryptor lost its key ratchet was
// silent for up to an hour, and the encrypt-stall rejoin exists to end that
// within ~10 s of speech. disgo's sender now skips a tick while the DAVE
// session is not ready — which is exactly what a missing key ratchet reports —
// so the frame is never encrypted, never fails, and the stall is never seen.
func TestEncryptStallReportedWhileDaveNotReady(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, opus.AudioChanBuf)
	speakingCaller(t, frames)
	stalls := startRatchetlessSpeaker(t, frames, withListener)

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

// Rejoining is only worth it while there is speech the bot cannot send. An
// idle bot is never rejoined, so one whose session never gets ready cannot
// cycle through a rejoin every few seconds for a whole raid.
func TestNoEncryptStallWhileIdle(t *testing.T) {
	t.Parallel()

	stalls := startRatchetlessSpeaker(t, make(chan []byte, opus.AudioChanBuf), withListener)

	time.Sleep(4 * stallFrames * voice.OpusFrameSizeMs * time.Millisecond)
	if n := stalls.Load(); n > 0 {
		t.Fatalf("an idle speaker with no key ratchet was reported stalled %d times: "+
			"it would be rejoined with nothing to say", n)
	}
}

// Discord does not make a lone bot's DAVE session ready (integration E22), so a
// speaker in an empty channel looks exactly like a stalled one — but nobody can
// hear it, and a rejoin lands it in the same empty channel. Production,
// 2026-10-26: 1,927 futile rejoins in a day.
func TestNoEncryptStallWhileAlone(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, opus.AudioChanBuf)
	speakingCaller(t, frames)
	stalls := startRatchetlessSpeaker(t, frames, alone)

	time.Sleep(4 * stallFrames * voice.OpusFrameSizeMs * time.Millisecond)
	if n := stalls.Load(); n > 0 {
		t.Fatalf("a speaker alone in its channel was reported stalled %d times: "+
			"it would be rejoined into the same empty channel", n)
	}
}

const stallFrames = 5

type channelCompany bool

const (
	alone        channelCompany = false
	withListener channelCompany = true
)

// startRatchetlessSpeaker connects a speaker fed from frames whose session has
// no key ratchet, with production sender wiring, and counts stall reports.
// company says whether anyone else is in the speaker's channel.
func startRatchetlessSpeaker(t *testing.T, frames chan []byte, company channelCompany) *atomic.Int64 {
	t.Helper()

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
	if company == withListener {
		conn.DAVE().AddUser(godave.UserID("1484911902801924237"))
	}
	conn.SetOpusFrameProvider(opus.NewVoiceProvider(frames, telemetry.OpusRecorder{}))
	t.Cleanup(func() { defaultSenders.Close(conn) })
	return &stalls
}
