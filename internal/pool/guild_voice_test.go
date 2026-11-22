package pool

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// Close records only the first context it gets: a retried Join closes twice.
func (c *hangingConn) Close(ctx context.Context) {
	select {
	case c.closeCtx <- ctx:
	default:
	}
	<-ctx.Done()
}

func (c *hangingConn) Gateway() voice.Gateway { return nil }

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
	synctest.Test(t, func(t *testing.T) {
		const (
			guildID   = snowflake.ID(851)
			channelID = snowflake.ID(852)
		)

		conn := newHangingConn()
		gv := NewGuildVoice(&hangingVoiceManager{conn: conn}, channelID)

		joined := make(chan error, 1)
		go func() { _, err := gv.Join(context.Background(), guildID); joined <- err }()

		openCtx := <-conn.openCtx
		deadline, ok := openCtx.Deadline()
		if !ok {
			t.Fatal("conn.Open got a context with no deadline: a handshake Discord never answers blocks the raid forever")
		}
		if d := time.Until(deadline); d != voiceJoinFirstTimeout {
			t.Errorf("join deadline is %v away, want %v", d, voiceJoinFirstTimeout)
		}

		time.Sleep(VoiceJoinCost - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-joined:
			t.Fatalf("Join returned (%v) before both handshakes and both leaves had run out", err)
		default:
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-joined:
			if err == nil {
				t.Error("Join returned a nil error after a handshake that never completed")
			}
		default:
			t.Errorf("Join did not return after %v: releasing the conn inherited the same unbounded wait", VoiceJoinCost)
		}
	})
}

// Stopping a raid mid-join cancels the context Join is holding. Releasing the
// conn still has to reach Discord, or the bot stays parked in the channel with
// nothing left to drive it out.
func TestGuildVoiceJoinReleasesConnAfterParentCancelled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			guildID   = snowflake.ID(851)
			channelID = snowflake.ID(852)
		)

		conn := newHangingConn()
		gv := NewGuildVoice(&hangingVoiceManager{conn: conn}, channelID)

		sessionCtx, cancel := context.WithCancel(context.Background())
		joined := make(chan error, 1)
		go func() { _, err := gv.Join(sessionCtx, guildID); joined <- err }()

		<-conn.openCtx
		cancel()

		closeCtx := <-conn.closeCtx
		if closeCtx.Err() != nil {
			t.Errorf("conn.Close got an already-dead context (%v): disgo sends the leave op through the rate limiter, which drops it, so the bot stays in the channel", closeCtx.Err())
		}
		<-joined
	})
}

// The tests above check that Join keeps to the timeouts; this one checks the
// timeouts themselves.
func TestVoiceTimeoutsStayWithinAUsersPatience(t *testing.T) {
	t.Parallel()
	if VoiceJoinCost > 20*time.Second {
		t.Errorf("a failing join takes up to %v, longer than the single 15 s attempt and 5 s cleanup the retry replaced", VoiceJoinCost)
	}
	// The slowest successful production join (2026-11-12 to 11-19) took 2.5 s;
	// a first attempt cut short below twice that would retry joins that work.
	if voiceJoinFirstTimeout < 5*time.Second {
		t.Errorf("first attempt gives up after %v, under twice the slowest join seen to succeed", voiceJoinFirstTimeout)
	}
}

// flakyConn is a voice.Conn whose first handshakes Discord never answers — it
// accepts the bot into the channel but sends no voice server — and whose later
// ones complete, the way a stuck join behaved in production on 2026-11-19.
type flakyConn struct {
	voice.Conn
	hangs  int32 // Opens that hang before one succeeds
	opens  atomic.Int32
	closes atomic.Int32
}

func (c *flakyConn) Open(ctx context.Context, _ snowflake.ID, _ bool, _ bool) error {
	if c.opens.Add(1) <= c.hangs {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *flakyConn) Close(context.Context)  { c.closes.Add(1) }
func (c *flakyConn) Gateway() voice.Gateway { return nil }

type flakyVoiceManager struct {
	hangingVoiceManager
	conn *flakyConn
}

func (m *flakyVoiceManager) CreateConn(snowflake.ID) voice.Conn { return m.conn }
func (m *flakyVoiceManager) GetConn(snowflake.ID) voice.Conn    { return m.conn }

// Every join that hung on 2026-11-19 went through when it was asked again, so
// one handshake Discord never answers must not cost the bot its place in the
// raid.
func TestGuildVoiceJoinRetriesAHandshakeThatHung(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			guildID   = snowflake.ID(701)
			channelID = snowflake.ID(702)
		)
		conn := &flakyConn{hangs: 1}
		gv := NewGuildVoice(&flakyVoiceManager{conn: conn}, channelID)

		got, err := gv.Join(context.Background(), guildID)

		if err != nil {
			t.Fatalf("Join failed after the first handshake hung (%d handshake(s) tried): %v", conn.opens.Load(), err)
		}
		if got != conn {
			t.Errorf("Join returned %v, want the conn", got)
		}
		if n := conn.closes.Load(); n == 0 {
			t.Error("the hung handshake's conn was not released before asking again: CreateConn would hand back a half-open conn")
		}
	})
}

// A /stop during the join cancels it; asking Discord again would only delay
// the teardown the stop asked for.
func TestGuildVoiceJoinDoesNotRetryAfterParentCancelled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			guildID   = snowflake.ID(711)
			channelID = snowflake.ID(712)
		)
		conn := &flakyConn{hangs: 2}
		gv := NewGuildVoice(&flakyVoiceManager{conn: conn}, channelID)

		ctx, cancel := context.WithTimeout(context.Background(), voiceJoinFirstTimeout/2)
		defer cancel()
		if _, err := gv.Join(ctx, guildID); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Join error = %v, want the parent's deadline", err)
		}
		if n := conn.opens.Load(); n != 1 {
			t.Errorf("handshakes tried = %d after the parent context ended, want 1", n)
		}
	})
}
