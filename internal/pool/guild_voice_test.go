package pool

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// hangingConn is a voice.Conn Discord never answers for: both Open and Close
// block until their context is done, exactly as disgo's connImpl does.
type hangingConn struct {
	voice.Conn
	openCtx  chan context.Context
	closeCtx chan context.Context
}

func newHangingConn() *hangingConn {
	return &hangingConn{
		openCtx:  make(chan context.Context, 1),
		closeCtx: make(chan context.Context, 1),
	}
}

func (c *hangingConn) Open(ctx context.Context, _ snowflake.ID, _ bool, _ bool) error {
	c.openCtx <- ctx
	<-ctx.Done()
	return ctx.Err()
}

func (c *hangingConn) Close(ctx context.Context) {
	c.closeCtx <- ctx
	<-ctx.Done()
}

// hangingVoiceManager hands out a single hangingConn.
type hangingVoiceManager struct{ conn *hangingConn }

func (m *hangingVoiceManager) HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate)   {}
func (m *hangingVoiceManager) HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate) {}
func (m *hangingVoiceManager) CreateConn(snowflake.ID) voice.Conn                     { return m.conn }
func (m *hangingVoiceManager) GetConn(snowflake.ID) voice.Conn                        { return m.conn }
func (m *hangingVoiceManager) Conns() iter.Seq[voice.Conn]                            { return func(func(voice.Conn) bool) {} }
func (m *hangingVoiceManager) RemoveConn(snowflake.ID)                                {}
func (m *hangingVoiceManager) Close(context.Context)                                  {}

// The raid context Join is given carries no deadline, so Join must bound the
// handshake — and the cleanup after a failed one — by itself.
func TestGuildVoiceJoinBoundsTheHandshake(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1430511050704289835)
		channelID = snowflake.ID(1529403555667251310)
	)

	conn := newHangingConn()
	gv := NewGuildVoice(&hangingVoiceManager{conn: conn}, channelID)
	gv.joinTimeout = 50 * time.Millisecond
	gv.cleanupTimeout = 50 * time.Millisecond

	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	joined := make(chan error, 1)
	go func() { _, err := gv.Join(sessionCtx, guildID); joined <- err }()

	var openCtx context.Context
	select {
	case openCtx = <-conn.openCtx:
	case <-time.After(5 * time.Second):
		t.Fatal("Join never reached conn.Open")
	}

	deadline, ok := openCtx.Deadline()
	if !ok {
		t.Fatal("conn.Open got a context with no deadline: a handshake Discord never answers blocks the raid forever")
	}
	if d := time.Until(deadline); d > gv.joinTimeout {
		t.Errorf("join deadline is %v away, want at most %v", d, gv.joinTimeout)
	}
	select {
	case err := <-joined:
		if err == nil {
			t.Error("Join returned a nil error after a handshake that never completed")
		}
	case <-time.After(time.Until(deadline) + gv.cleanupTimeout + 5*time.Second):
		t.Error("Join did not return after its deadline: releasing the conn inherited the same unbounded wait")
	}
}

// Stopping a raid mid-join cancels the context Join is holding. Releasing the
// conn still has to reach Discord, or the bot stays parked in the channel with
// nothing left to drive it out.
func TestGuildVoiceJoinReleasesConnAfterParentCancelled(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1430511050704289835)
		channelID = snowflake.ID(1529403555667251310)
	)

	conn := newHangingConn()
	gv := NewGuildVoice(&hangingVoiceManager{conn: conn}, channelID)
	gv.cleanupTimeout = 50 * time.Millisecond

	sessionCtx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() { _, err := gv.Join(sessionCtx, guildID); joined <- err }()

	<-conn.openCtx
	cancel()

	var closeCtx context.Context
	select {
	case closeCtx = <-conn.closeCtx:
	case <-time.After(5 * time.Second):
		t.Fatal("Join never released the conn")
	}
	if closeCtx.Err() != nil {
		t.Errorf("conn.Close got an already-dead context (%v): disgo sends the leave op through the rate limiter, which drops it, so the bot stays in the channel", closeCtx.Err())
	}

	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Error("Join did not return after the parent was cancelled")
	}
}

// Guards the values production actually runs with, which the injected
// timeouts above say nothing about.
func TestVoiceTimeoutsStayWithinAUsersPatience(t *testing.T) {
	t.Parallel()
	if total := VoiceJoinTimeout + voiceCleanupTimeout; total > time.Minute {
		t.Errorf("a failing join takes up to %v, want at most 1m", total)
	}
}
