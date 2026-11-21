package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
)

type gatewayConn struct {
	voice.Conn
	gw voice.Gateway
}

func (c gatewayConn) Gateway() voice.Gateway { return c.gw }

type fakeGateway struct {
	voice.Gateway
	name string
}

func TestVoiceReadyRoutesByGateway(t *testing.T) {
	t.Parallel()

	w := newVoiceReadyWatchers()
	gwA, gwB := &fakeGateway{name: "a"}, &fakeGateway{name: "b"}
	connA, connB := gatewayConn{gw: gwA}, gatewayConn{gw: gwB}

	var readyA, readyB int
	w.watch(connA, func() { readyA++ })
	w.watch(connB, func() { readyB++ })

	w.handle(gwA, voice.OpcodeReady, 0, nil)
	w.handle(gwA, voice.OpcodeSpeaking, 0, nil)
	if readyA != 1 || readyB != 0 {
		t.Fatalf("after one Ready on A: a=%d b=%d, want a=1 b=0", readyA, readyB)
	}

	w.forget(connA)
	w.handle(gwA, voice.OpcodeReady, 0, nil)
	w.handle(gwB, voice.OpcodeReady, 0, nil)
	if readyA != 1 || readyB != 1 {
		t.Fatalf("after forgetting A: a=%d b=%d, want a=1 b=1", readyA, readyB)
	}
}

// closingVoiceServer readies the gateway, then closes it with code once
// closeNow is closed.
func closingVoiceServer(t *testing.T, code int, closeNow <-chan struct{}) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if err := conn.WriteJSON(map[string]any{"op": voice.OpcodeResumed}); err != nil {
			return
		}
		select {
		case <-closeNow:
		case <-r.Context().Done():
			return
		}
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, "disconnected"))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func isWatched(gw voice.Gateway) bool {
	defaultReadyWatchers.mu.Lock()
	defer defaultReadyWatchers.mu.Unlock()
	_, ok := defaultReadyWatchers.handlers[gw]
	return ok
}

// A conn disgo closes on its own — the voice server disconnecting the bot —
// is already gone from the manager when our teardown runs, so Leave finds
// nothing to forget and the watch would outlive the conn it holds.
func TestReadyWatchForgottenWhenDisgoClosesTheConn(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(841)
		userID    = snowflake.ID(842)
		channelID = snowflake.ID(843)
	)

	closeNow := make(chan struct{})
	srv := closingVoiceServer(t, voice.GatewayCloseEventCodeDisconnected.Code, closeNow)
	vm := newVoiceManager(userID, sessionOf(&epochSession{preparing: make(chan struct{})}), withDialer(trustingDialer(srv)))
	conn := openVoiceConn(t, vm, srv, guildID, userID, channelID)
	gw := conn.Gateway()

	OnVoiceReady(conn, func() {})
	if !isWatched(gw) {
		t.Fatal("OnVoiceReady registered no watch")
	}

	close(closeNow)
	if !eventually(5*time.Second, func() bool { return vm.GetConn(guildID) == nil }) {
		t.Fatal("disgo never closed the conn after the voice server disconnected it")
	}

	NewGuildVoice(vm, channelID).Leave(context.Background(), guildID)
	if isWatched(gw) {
		t.Error("the ready watch outlived its conn: disgo closed the conn itself, so Leave had nothing to forget")
	}
}
