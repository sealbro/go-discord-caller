package pool

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
)

// acceptingVoiceServer completes the websocket upgrade and then says nothing,
// which is all the gateway needs to get past its dial.
func acceptingVoiceServer(t *testing.T) string {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = conn.Close() })
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "https://")
}

func asError(recovered any) error {
	err, _ := recovered.(error)
	return err
}

func nilChannelState(endpoint string) voice.State {
	return voice.State{
		GuildID:   snowflake.ID(1430511050704289835),
		UserID:    snowflake.ID(1529403555667251310),
		ChannelID: nil, // what disgo records once Discord reports a disconnect
		SessionID: "session",
		Token:     "token",
		Endpoint:  endpoint,
	}
}

func testGatewayOpts() []voice.GatewayConfigOpt {
	return []voice.GatewayConfigOpt{
		voice.WithGatewayLogger(slog.New(slog.DiscardHandler)),
		voice.WithGatewayDialer(&websocket.Dialer{
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
			HandshakeTimeout: 5 * time.Second,
		}),
	}
}

// Tripwire: when this fails, upstream has fixed the dereference and safeGateway
// should be deleted.
func TestDisgoVoiceGatewayStillPanicsOnNilChannelID(t *testing.T) {
	endpoint := acceptingVoiceServer(t)

	gateway := voice.NewGateway(
		godave.NewNoopSession(slog.New(slog.DiscardHandler), "", nil),
		func(voice.Gateway, voice.Opcode, int, voice.GatewayMessageData) {},
		func(voice.Gateway, error, bool) {},
		testGatewayOpts()...,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The recovered value is checked, not just its presence, so a panic from
	// elsewhere in Open cannot keep the tripwire quiet.
	recovered := func() (recovered any) {
		defer func() { recovered = recover() }()
		_ = gateway.Open(ctx, nilChannelState(endpoint))
		return nil
	}()

	var runtimeErr runtime.Error
	if !errors.As(asError(recovered), &runtimeErr) || !strings.Contains(runtimeErr.Error(), "nil pointer dereference") {
		t.Errorf("disgo no longer dereferences a nil ChannelID (recovered %v): delete safeGateway and SafeGatewayOpt", recovered)
	}
}

func TestSafeGatewayRefusesNilChannelID(t *testing.T) {
	endpoint := acceptingVoiceServer(t)

	gateway := NewSafeGateway(
		godave.NewNoopSession(slog.New(slog.DiscardHandler), "", nil),
		func(voice.Gateway, voice.Opcode, int, voice.GatewayMessageData) {},
		func(voice.Gateway, error, bool) {},
		testGatewayOpts()...,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("safeGateway panicked instead of returning an error: %v", r)
			}
		}()
		return gateway.Open(ctx, nilChannelState(endpoint))
	}()

	if err == nil {
		t.Error("Open accepted a state with no channel; the bot has already left")
	}
}
