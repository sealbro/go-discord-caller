package pool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

// stubConn is a voice.Conn used as a registry key. It reports a ready DAVE
// session so the sender pulls frames; anything else would be called only once a
// frame is produced, and blockedProvider never produces one.
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

// pollsWithin reports how many times the provider was polled over d of a
// synctest bubble's time, counting the polls due at its last instant.
func (p *blockedProvider) pollsWithin(d time.Duration) int64 {
	synctest.Wait()
	before := p.calls.Load()
	time.Sleep(d)
	synctest.Wait()
	return p.calls.Load() - before
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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
// The registry exists so a conn's removal can close the sender that
// Conn.Close leaves behind. This asserts that closing the registry entry
// actually stops the goroutine.
func TestAudioSenderRegistryStopsSpinningSender(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := NewAudioSenderRegistry()
		conn := &stubConn{}
		p := newBlockedProvider()

		sender := r.CreateFunc()(discardLogger(), p, conn)
		sender.Open()
		t.Cleanup(func() { stopInBubble(sender) })

		// Teardown closes the provider, as manager.VoiceConnSetup.Apply's cleanup does.
		p.Close()

		// Precondition: without intervention the sender spins at the 20 ms frame
		// cadence. If this ever stops being true, disgo has fixed the underlying
		// bug and this whole workaround can be deleted.
		const window = 200 * time.Millisecond
		if spun, want := p.pollsWithin(window), int64(window/frameTime); spun != want {
			t.Fatalf("precondition failed: sender polled a closed provider %d times in %v, want %d (one per frame) — "+
				"if disgo no longer spins on provider errors, AudioSenderRegistry, "+
				"SafeAudioSenderOpt, and the sender cleanup in pool.newConn "+
				"are obsolete and should be removed", spun, window, want)
		}

		// This is what newConn's removal hook does after conn.Close.
		r.Close(conn)

		if got := p.pollsWithin(window); got != 0 {
			t.Fatalf("after registry Close the sender was still polling %d times in %v — "+
				"the sender goroutine outlived teardown and keeps logging an ERROR every tick",
				got, window)
		}

		// Close is idempotent and safe for an unknown conn.
		r.Close(conn)
		r.Close(&stubConn{})
	})
}

// TestAudioSenderRegistryCloseAll verifies the shutdown backstop stops senders
// whose conn was never removed.
func TestAudioSenderRegistryCloseAll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := NewAudioSenderRegistry()

		providers := make([]*blockedProvider, 3)
		for i := range providers {
			p := newBlockedProvider()
			providers[i] = p
			sender := r.CreateFunc()(discardLogger(), p, &stubConn{})
			sender.Open()
			t.Cleanup(func() { stopInBubble(sender) })
			p.Close()
		}

		r.CloseAll()

		for i, p := range providers {
			if got := p.pollsWithin(time.Second); got != 0 {
				t.Errorf("sender %d still polling %d times a second after CloseAll", i, got)
			}
		}
	})
}

// TestAudioSenderCloseBeforeOpen covers the Close-before-Open hazard:
// disgo's defaultAudioSender.Close calls s.cancelFunc unconditionally, and
// that field stays nil until the goroutine Open spawns gets scheduled. Join
// timeouts make teardown of a never-started sender routine, and an unrecovered
// panic there would kill the process.
func TestAudioSenderCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := NewAudioSenderRegistry()
		conn := &stubConn{}
		// Register a sender but never Open it, so cancelFunc is never set.
		r.CreateFunc()(discardLogger(), newBlockedProvider(), conn)

		start := time.Now()
		// Must not panic.
		r.Close(conn)
		if waited := time.Since(start); waited != senderStartTimeout {
			t.Errorf("Close of a never-opened sender returned after %v, want the %v start timeout", waited, senderStartTimeout)
		}
	})
}

// TestDisgoAudioSenderStillPanicsOnCloseBeforeOpen is the tripwire for the
// upstream bug. When it fails, disgo has fixed defaultAudioSender.Close and the
// recover inside safeAudioSender.Close is obsolete.
//
// UPSTREAM-TRIPWIRE(disgo v0.20.0): delete safeAudioSender's recover.
func TestDisgoAudioSenderStillPanicsOnCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("disgo's defaultAudioSender.Close no longer panics before Open — " +
				"the recover in safeAudioSender.Close is obsolete and should be removed")
		}
	}()
	voice.NewAudioSender(discardLogger(), newBlockedProvider(), &stubConn{}).Close()
}

// readySession is a DAVE session that reports ready, so an audio sender polls
// its provider every tick.
type readySession struct{ godave.Session }

func (readySession) SetChannelID(godave.ChannelID) {}
func (readySession) Ready() bool                   { return true }
func (readySession) Close() error                  { return nil }

func (stubConn) DAVE() godave.Session { return readySession{} }

// A conn disgo closes on its own — the voice server disconnecting the bot —
// is already gone from the manager when our teardown runs, so Leave cannot
// reach its sender. Once teardown closes the provider, that sender spins on it.
func TestAudioSenderStoppedWhenDisgoClosesTheConn(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(301)
		userID    = snowflake.ID(302)
		channelID = snowflake.ID(303)
	)

	closeNow := make(chan struct{})
	srv := closingVoiceServer(t, voice.GatewayCloseEventCodeDisconnected.Code, closeNow)
	vm := newVoiceManager(userID, sessionOf(readySession{}), withDialer(trustingDialer(srv)))
	conn := openVoiceConn(t, vm, srv, guildID, userID, channelID)
	p := newBlockedProvider()
	conn.SetOpusFrameProvider(p)

	close(closeNow)
	if !eventually(5*time.Second, func() bool { return vm.GetConn(guildID) == nil }) {
		t.Fatal("disgo never closed the conn after the voice server disconnected it")
	}

	NewGuildVoice(vm, channelID).Leave(context.Background(), guildID)
	p.Close()
	const window = 100 * time.Millisecond
	before := p.calls.Load()
	time.Sleep(window)
	if n := p.calls.Load() - before; n > 0 {
		t.Errorf("audio sender polled its closed provider %d times in %v: disgo closed the conn itself, so Leave never stopped the sender", n, window)
	}
}
