package pool

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
)

// gatedConn holds every write made after arm until release, so a test can
// freeze the voice gateway at the moment it is writing.
type gatedConn struct {
	net.Conn
	armed     atomic.Bool
	blocked   chan struct{}
	blockOnce sync.Once
	released  chan struct{}
}

func (c *gatedConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		c.blockOnce.Do(func() { close(c.blocked) })
		<-c.released
	}
	return c.Conn.Write(p)
}

// epochSession answers a DAVE prepare-epoch by sending its key package, as
// golibdave does, from the gateway's listen goroutine.
type epochSession struct {
	godave.Session
	callbacks godave.Callbacks
	preparing chan struct{}
}

func (*epochSession) SetChannelID(godave.ChannelID) {}
func (*epochSession) Close() error                  { return nil }

func (s *epochSession) OnDavePrepareEpoch(int, uint16) {
	close(s.preparing)
	_ = s.callbacks.SendMLSKeyPackage([]byte{0})
}

// epochVoiceServer brings the gateway to ready, then sends a DAVE prepare-epoch
// once sendEpoch is closed.
func epochVoiceServer(t *testing.T, sendEpoch <-chan struct{}) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Resumed readies the gateway the same as Ready, without disgo's conn
		// opening a UDP socket to the address Ready carries.
		if err := conn.WriteJSON(map[string]any{"op": voice.OpcodeResumed}); err != nil {
			return
		}
		select {
		case <-sendEpoch:
		case <-r.Context().Done():
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"op": voice.OpcodeDavePrepareEpoch,
			"d":  map[string]any{"epoch": 1, "protocol_version": 1},
		}); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// statusLockHeld reports whether something is holding the gateway's status
// lock, which Status takes and Send keeps for the whole of its write.
func statusLockHeld(gw voice.Gateway) bool {
	done := make(chan struct{})
	go func() {
		gw.Status()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(50 * time.Millisecond):
		return true
	}
}

// disgo's voice gateway takes its two locks in opposite orders: Send holds the
// status lock while it waits for the connection lock, and Close holds the
// connection lock while it waits for the status lock. A DAVE epoch arriving
// while the bot leaves voice deadlocks the two, and the stuck Close also wedges
// the bot's main gateway, which handles the voice-state update that closes the
// conn. Seen as an integration run hanging until the 15 m test timeout.
func TestLeaveDoesNotDeadlockWithDaveEpoch(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1430511050704289835)
		userID    = snowflake.ID(1529403555667251310)
		channelID = snowflake.ID(1430511051383767101)
	)

	sendEpoch := make(chan struct{})
	srv := epochVoiceServer(t, sendEpoch)

	gate := &gatedConn{blocked: make(chan struct{}), released: make(chan struct{})}
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			gate.Conn = conn
			return gate, nil
		},
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
	}

	dave := &epochSession{preparing: make(chan struct{})}
	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		userID,
		voice.WithDaveSessionCreateFunc(func(_ *slog.Logger, _ godave.UserID, callbacks godave.Callbacks) godave.Session {
			dave.callbacks = callbacks
			return dave
		}),
		SafeAudioSenderOpt(),
		VoiceReadyOpt(),
		SerialGatewayOpt(),
		voice.WithConnConfigOpts(voice.WithConnGatewayConfigOpts(voice.WithGatewayDialer(dialer))),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	conn := vm.CreateConn(guildID)
	gw := conn.Gateway()

	openCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gw.Open(openCtx, voice.State{
		GuildID:   guildID,
		UserID:    userID,
		ChannelID: channelID,
		SessionID: "session",
		Token:     "token",
		Endpoint:  strings.TrimPrefix(srv.URL, "https://"),
	}); err != nil {
		t.Fatalf("open voice gateway: %v", err)
	}

	gate.armed.Store(true)
	left := make(chan struct{})
	go func() {
		NewGuildVoice(vm, channelID).Leave(context.Background(), guildID)
		close(left)
	}()

	select {
	case <-gate.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Leave never wrote the close frame")
	}

	close(sendEpoch)
	select {
	case <-dave.preparing:
	case <-time.After(5 * time.Second):
		t.Fatal("the DAVE prepare-epoch never reached the session")
	}
	// Unfixed, the Send takes the status lock within milliseconds, and releasing
	// the gate before it does would let Close finish first. Fixed, it waits
	// outside disgo's locks and never takes it.
	deadline := time.Now().Add(time.Second)
	for !statusLockHeld(gw) && time.Now().Before(deadline) {
	}

	close(gate.released)
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("Leave never returned: closing the voice gateway deadlocked with the DAVE key-package Send")
	}
}
