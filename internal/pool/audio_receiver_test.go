package pool

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

// notReadyDaveSession reports a DAVE session that never becomes ready — the
// bot's encryptor is in passthrough or has lost its key ratchet — and counts
// how often it is asked. After release, Ready parks forever so the receiver
// goroutine, which nothing in disgo stops, stays off the CPU.
type notReadyDaveSession struct {
	godave.Session
	checks  atomic.Int64
	release chan struct{}
}

func (s *notReadyDaveSession) Ready() bool {
	select {
	case <-s.release:
		select {}
	default:
	}
	s.checks.Add(1)
	return false
}

func (*notReadyDaveSession) SetChannelID(godave.ChannelID) {}
func (*notReadyDaveSession) Close() error                  { return nil }

type discardReceiver struct{}

func (discardReceiver) ReceiveOpusFrame(snowflake.ID, *voice.Packet) error { return nil }
func (discardReceiver) CleanupUser(snowflake.ID)                           {}
func (discardReceiver) Close()                                             {}

// Production, 2026-10-25: a host running one raid sat at 100% CPU for hours
// and every bot's gateway heartbeat rose from ~110 ms to ~545 ms. Until disgo
// v0.20.0 the receive loop returned straight away while the conn's DAVE
// session was not ready, so a conn whose session never got ready spun a core.
func TestAudioReceiverDoesNotSpinWhileDaveNotReady(t *testing.T) {
	t.Parallel()

	const guildID = snowflake.ID(1430511050704289835)

	session := &notReadyDaveSession{release: make(chan struct{})}
	t.Cleanup(func() { close(session.release) })

	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		snowflake.ID(1529403555667251310),
		voice.WithDaveSessionCreateFunc(func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
			return session
		}),
		SafeAudioSenderOpt(),
		VoiceConnOpt(),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	vm.CreateConn(guildID).SetOpusFrameReceiver(discardReceiver{})

	const window = 200 * time.Millisecond
	before := session.checks.Load()
	time.Sleep(window)
	got := session.checks.Load() - before

	// One check per 20 ms frame would be 10; 50 leaves room for jitter while
	// still failing anything close to a busy loop.
	if got > 50 {
		t.Fatalf("audio receiver checked DAVE readiness %d times in %v (%.0f/sec) while the session "+
			"was not ready: it busy-loops and pins a CPU core", got, window, float64(got)/window.Seconds())
	}
}

// disgo's connImpl.Close leaves the audio receiver running. It still exits on
// its own once ReadPacket fails on the closed socket, but a receiver whose DAVE
// session never got ready never reaches that read, and golibdave's Close does
// not change readiness, so it outlives its conn for the life of the process.
func TestAudioReceiverStoppedWhenConnLeaves(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1430511050704289835)
		channelID = snowflake.ID(1430511051383767101)
	)

	session := &notReadyDaveSession{release: make(chan struct{})}
	t.Cleanup(func() { close(session.release) })

	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		snowflake.ID(1529403555667251310),
		voice.WithDaveSessionCreateFunc(func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
			return session
		}),
		SafeAudioSenderOpt(),
		VoiceConnOpt(),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	vm.CreateConn(guildID).SetOpusFrameReceiver(discardReceiver{})

	NewGuildVoice(vm, channelID).Leave(context.Background(), guildID)
	if vm.GetConn(guildID) != nil {
		t.Fatal("precondition failed: Leave did not remove the conn")
	}

	// Let a check that was in flight during Leave finish.
	time.Sleep(50 * time.Millisecond)
	const window = 200 * time.Millisecond
	before := session.checks.Load()
	time.Sleep(window)
	if got := session.checks.Load() - before; got > 0 {
		t.Fatalf("audio receiver checked DAVE readiness %d times in %v after its conn left: "+
			"the receiver goroutine outlived the conn", got, window)
	}
}

// UPSTREAM-TRIPWIRE(disgo v0.20.0): when this fails, disgo stops the audio
// receiver when it closes the conn and newOwnedAudioReceiver is obsolete.
func TestDisgoAudioReceiverStillOutlivesItsConn(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1430511050704289835)
		channelID = snowflake.ID(1430511051383767101)
	)

	session := &notReadyDaveSession{release: make(chan struct{})}
	t.Cleanup(func() { close(session.release) })

	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		snowflake.ID(1529403555667251310),
		voice.WithDaveSessionCreateFunc(func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
			return session
		}),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	vm.CreateConn(guildID).SetOpusFrameReceiver(discardReceiver{})

	NewGuildVoice(vm, channelID).Leave(context.Background(), guildID)
	if vm.GetConn(guildID) != nil {
		t.Fatal("precondition failed: Leave did not remove the conn")
	}

	time.Sleep(50 * time.Millisecond)
	before := session.checks.Load()
	time.Sleep(200 * time.Millisecond)
	if session.checks.Load() == before {
		t.Error("disgo's audio receiver stopped after its conn was closed, so newOwnedAudioReceiver " +
			"and the receiver registry should be removed")
	}
}
