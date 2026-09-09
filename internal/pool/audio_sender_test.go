package pool

import (
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
)

// stubConn is a voice.Conn used only as a registry key. The audio sender
// dereferences its Conn solely once a frame is produced, and blockedProvider
// never produces one, so the embedded nil interface is never called.
type stubConn struct{ voice.Conn }

// blockedProvider mimics opus.EmptyVoiceProvider: ProvideOpusFrame blocks
// until Close, then returns an error on every subsequent call. It counts calls
// so a spinning sender is visible.
type blockedProvider struct {
	voice.OpusFrameProvider
	done  chan struct{}
	calls atomic.Int64
}

func newBlockedProvider() *blockedProvider {
	return &blockedProvider{done: make(chan struct{})}
}

func (p *blockedProvider) ProvideOpusFrame() ([]byte, error) {
	p.calls.Add(1)
	<-p.done
	return nil, fmt.Errorf("empty voice provider closed")
}

func (p *blockedProvider) Close() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// pollsWithin reports how many times the provider was polled over d.
func (p *blockedProvider) pollsWithin(d time.Duration) int64 {
	before := p.calls.Load()
	time.Sleep(d)
	return p.calls.Load() - before
}

// TestAudioSenderRegistryStopsSpinningSender is the regression test for the
// 2026-09-07 incident: one speaker bot logged "error while reading opus frame"
// / "empty voice provider closed" at 50 lines/second for 2h50m (~510k ERROR
// lines) after its voice connection was torn down.
//
// disgo's connImpl.Close does not stop the audio sender, and
// defaultAudioSender.send logs a provider error without breaking its 20 ms
// loop. Once teardown closes the provider, the surviving sender polls it every
// tick, gets an instant error, and logs it forever.
//
// The registry exists so GuildVoice.Leave can close the sender that
// Conn.Close leaves behind. This asserts that closing the registry entry
// actually stops the goroutine.
func TestAudioSenderRegistryStopsSpinningSender(t *testing.T) {
	t.Parallel()

	r := NewAudioSenderRegistry()
	conn := &stubConn{}
	p := newBlockedProvider()

	sender := r.CreateFunc()(slog.New(slog.NewTextHandler(io.Discard, nil)), p, conn)
	sender.Open()
	t.Cleanup(sender.Close)

	// Teardown closes the provider, as manager.VoiceConnSetup.Apply's cleanup does.
	p.Close()

	// Precondition: without intervention the sender spins at the 20 ms frame
	// cadence. If this ever stops being true, disgo has fixed the underlying
	// bug and this whole workaround can be deleted.
	if spun := p.pollsWithin(200 * time.Millisecond); spun < 5 {
		t.Fatalf("precondition failed: sender polled a closed provider only %d times in 200ms — "+
			"disgo no longer spins on provider errors, so AudioSenderRegistry, "+
			"SafeAudioSenderOpt, and the CloseAudioSender call in GuildVoice.Leave "+
			"are obsolete and should be removed", spun)
	}

	// This is what GuildVoice.Leave does after conn.Close.
	r.Close(conn)

	const window = 400 * time.Millisecond
	// Allow one in-flight tick that raced the close.
	if got := p.pollsWithin(window); got > 1 {
		t.Fatalf("after registry Close the sender was still polling %d times in %v (%.0f calls/sec) — "+
			"the sender goroutine outlived teardown and keeps logging an ERROR every tick",
			got, window, float64(got)/window.Seconds())
	}

	// Close is idempotent and safe for an unknown conn.
	r.Close(conn)
	r.Close(&stubConn{})
}

// TestAudioSenderRegistryCloseAll verifies the shutdown backstop stops senders
// whose conn never went through GuildVoice.Leave.
func TestAudioSenderRegistryCloseAll(t *testing.T) {
	t.Parallel()

	r := NewAudioSenderRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	providers := make([]*blockedProvider, 3)
	for i := range providers {
		p := newBlockedProvider()
		providers[i] = p
		sender := r.CreateFunc()(logger, p, &stubConn{})
		sender.Open()
		t.Cleanup(sender.Close)
		p.Close()
	}

	r.CloseAll()

	const window = 300 * time.Millisecond
	for i, p := range providers {
		if got := p.pollsWithin(window); got > 1 {
			t.Errorf("sender %d still polling %d times in %v after CloseAll", i, got, window)
		}
	}
}

// TestAudioSenderCloseBeforeOpen covers the Close-before-Open hazard:
// disgo's defaultAudioSender.Close calls s.cancelFunc unconditionally, and
// that field stays nil until the goroutine Open spawns gets scheduled. Join
// timeouts make teardown of a never-started sender routine, and an unrecovered
// panic there would kill the process — the same failure mode safeUDPConn
// guards for UDP sockets.
func TestAudioSenderCloseBeforeOpen(t *testing.T) {
	// Not parallel: senderStartTimeout is package state.
	orig := senderStartTimeout
	senderStartTimeout = 50 * time.Millisecond
	t.Cleanup(func() { senderStartTimeout = orig })

	r := NewAudioSenderRegistry()
	conn := &stubConn{}
	// Register a sender but never Open it, so cancelFunc is never set.
	r.CreateFunc()(slog.New(slog.NewTextHandler(io.Discard, nil)), newBlockedProvider(), conn)

	// Must not panic.
	r.Close(conn)
}

// TestDisgoAudioSenderStillPanicsOnCloseBeforeOpen is the tripwire for the
// upstream bug. When it fails, disgo has fixed defaultAudioSender.Close and
// the recover inside safeAudioSender.Close is obsolete.
func TestDisgoAudioSenderStillPanicsOnCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("disgo's defaultAudioSender.Close no longer panics before Open — " +
				"the recover in safeAudioSender.Close is obsolete and should be removed")
		}
	}()
	voice.NewAudioSender(slog.New(slog.NewTextHandler(io.Discard, nil)), newBlockedProvider(), &stubConn{}).Close()
}
