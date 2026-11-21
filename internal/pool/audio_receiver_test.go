package pool

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

// spinChecks is how many readiness checks at one instant of a synctest
// bubble's clock count as a busy loop: a loop that never sleeps never lets the
// clock move.
const spinChecks = 100

// notReadyDaveSession reports a DAVE session that never becomes ready — the
// bot's encryptor is in passthrough or has lost its key ratchet — and counts
// how often it is asked. Ready ends the receiver goroutine asking it once the
// session is stopped, or once it spins: nothing in disgo stops the receiver of
// a conn that never left, and a bubble cannot end while it runs.
type notReadyDaveSession struct {
	godave.Session
	checks  atomic.Int64
	spun    atomic.Bool
	release chan struct{}

	lastCheck   time.Time
	sameInstant int
}

func (s *notReadyDaveSession) Ready() bool {
	select {
	case <-s.release:
		runtime.Goexit()
	default:
	}
	s.checks.Add(1)
	if now := time.Now(); now.Equal(s.lastCheck) {
		s.sameInstant++
	} else {
		s.lastCheck, s.sameInstant = now, 0
	}
	if s.sameInstant == spinChecks {
		s.spun.Store(true)
		runtime.Goexit()
	}
	return false
}

func (*notReadyDaveSession) SetChannelID(godave.ChannelID) {}
func (*notReadyDaveSession) Close() error                  { return nil }

// checksWithin reports how many times the session was checked over d of the
// bubble's time, counting the checks due at its last instant.
func (s *notReadyDaveSession) checksWithin(d time.Duration) int64 {
	synctest.Wait()
	before := s.checks.Load()
	time.Sleep(d)
	synctest.Wait()
	return s.checks.Load() - before
}

type discardReceiver struct{}

func (discardReceiver) ReceiveOpusFrame(snowflake.ID, *voice.Packet) error { return nil }
func (discardReceiver) CleanupUser(snowflake.ID)                           {}
func (discardReceiver) Close()                                             {}

const (
	receiverGuildID   = snowflake.ID(401)
	receiverChannelID = snowflake.ID(402)
	receiverBotID     = snowflake.ID(403)
)

// startNotReadyReceiver starts the audio receiver of a conn, built by newVM,
// whose DAVE session never gets ready.
func startNotReadyReceiver(t *testing.T, newVM func(snowflake.ID, godave.SessionCreateFunc, ...voice.ManagerConfigOpt) voice.Manager) (voice.Manager, *notReadyDaveSession) {
	t.Helper()

	session := &notReadyDaveSession{release: make(chan struct{})}
	t.Cleanup(func() {
		close(session.release)
		time.Sleep(frameTime)
	})
	vm := newVM(receiverBotID, sessionOf(session))
	vm.CreateConn(receiverGuildID).SetOpusFrameReceiver(discardReceiver{})
	synctest.Wait()
	return vm, session
}

func leaveReceiverChannel(t *testing.T, vm voice.Manager) {
	t.Helper()
	NewGuildVoice(vm, receiverChannelID).Leave(context.Background(), receiverGuildID)
	if vm.GetConn(receiverGuildID) != nil {
		t.Fatal("precondition failed: Leave did not remove the conn")
	}
}

// Production, 2026-10-25: a host running one raid sat at 100% CPU for hours
// and every bot's gateway heartbeat rose from ~110 ms to ~545 ms. Until disgo
// v0.20.0 the receive loop returned straight away while the conn's DAVE
// session was not ready, so a conn whose session never got ready spun a core.
func TestAudioReceiverDoesNotSpinWhileDaveNotReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		_, session := startNotReadyReceiver(t, newVoiceManager)

		const window = 200 * time.Millisecond
		got := session.checksWithin(window)
		if session.spun.Load() {
			t.Fatalf("audio receiver checked DAVE readiness %d times without any time passing while the session "+
				"was not ready: it busy-loops and pins a CPU core", spinChecks)
		}
		if want := int64(window / frameTime); got != want {
			t.Fatalf("audio receiver checked DAVE readiness %d times in %v while the session was not ready, want %d (one per frame)",
				got, window, want)
		}
	})
}

// disgo's connImpl.Close leaves the audio receiver running. It still exits on
// its own once ReadPacket fails on the closed socket, but a receiver whose DAVE
// session never got ready never reaches that read, and golibdave's Close does
// not change readiness, so it outlives its conn for the life of the process.
func TestAudioReceiverStoppedWhenConnLeaves(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		vm, session := startNotReadyReceiver(t, newVoiceManager)
		leaveReceiverChannel(t, vm)

		const window = time.Second
		if got := session.checksWithin(window); got > 0 {
			t.Fatalf("audio receiver checked DAVE readiness %d times in %v after its conn left: "+
				"the receiver goroutine outlived the conn", got, window)
		}
	})
}

// UPSTREAM-TRIPWIRE(disgo v0.20.0): when this fails, disgo stops the audio
// receiver when it closes the conn and newOwnedAudioReceiver is obsolete.
func TestDisgoAudioReceiverStillOutlivesItsConn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		vm, session := startNotReadyReceiver(t, newBareVoiceManager)
		leaveReceiverChannel(t, vm)

		if session.checksWithin(time.Second) == 0 {
			t.Error("disgo's audio receiver stopped after its conn was closed, so newOwnedAudioReceiver " +
				"and the receiver registry should be removed")
		}
	})
}
