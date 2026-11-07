//go:build !race

// disgo v0.20.0 reads Gateway.heartbeatCancel in CloseWithCode while the
// heartbeat goroutine writes it, so every reconnect trips the race detector
// regardless of our code. Drop this constraint once that is fixed upstream.

package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/gorilla/websocket"

	"github.com/sealbro/go-discord-caller/internal/dave"
)

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

type gatewayFrame struct {
	Op int             `json:"op"`
	T  string          `json:"t,omitempty"`
	S  int             `json:"s,omitempty"`
	D  json.RawMessage `json:"d"`
}

// identifyingGateway accepts every IDENTIFY with READY and then invalidates the
// session, so a client that reconnects at once is identified over and over. It
// sends each IDENTIFY's arrival time on the returned channel.
func identifyingGateway(t *testing.T) (wsURL string, identifies <-chan time.Time) {
	t.Helper()
	arrivals := make(chan time.Time, 64)
	upgrader := websocket.Upgrader{}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		send := func(f gatewayFrame) bool { return conn.WriteJSON(f) == nil }
		if !send(gatewayFrame{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":41250}`)}) {
			return
		}
		for {
			var in gatewayFrame
			if err := conn.ReadJSON(&in); err != nil {
				return
			}
			switch in.Op {
			case opHeartbeat:
				if !send(gatewayFrame{Op: opHeartbeatAck}) {
					return
				}
			case opIdentify:
				arrivals <- time.Now()
				ready := `{"v":10,"user":{"id":"1","username":"owner","bot":true},"guilds":[],` +
					`"session_id":"s","resume_gateway_url":"` + strings.Replace(srv.URL, "http", "ws", 1) + `",` +
					`"application":{"id":"1","flags":0}}`
				if !send(gatewayFrame{Op: opDispatch, T: "READY", S: 1, D: json.RawMessage(ready)}) {
					return
				}
				time.Sleep(500 * time.Millisecond)
				if !send(gatewayFrame{Op: opInvalidSession, D: json.RawMessage(`false`)}) {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http", "ws", 1), arrivals
}

// Production, 2026-11-07: a gateway that kept dropping the owner after READY was
// re-identified every few seconds for 90 minutes. That used up Discord's 1,000
// IDENTIFYs a day, which resets the bot token and left the bot offline. disgo
// restarts its reconnect backoff at one second after every READY, so a gateway
// that is ready but unstable is never slowed down.
func TestGatewaySlowsDownWhenSessionKeepsBeingInvalidated(t *testing.T) {
	wsURL, identifies := identifyingGateway(t)

	client, err := NewOwnerClient("MQ.fake.token", dave.NewStats(), dave.NewEncryptStalls(),
		bot.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		bot.WithGatewayConfigOpts(gateway.WithURL(wsURL), gateway.WithCompression(gateway.CompressionNone)),
	)
	if err != nil {
		t.Fatalf("NewOwnerClient: %v", err)
	}
	openCtx, cancelOpen := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOpen()
	if err := client.OpenGateway(openCtx); err != nil {
		t.Fatalf("OpenGateway: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.Close(closeCtx)
	})

	const wanted = 4
	var arrived []time.Time
	deadline := time.After(40 * time.Second)
	for len(arrived) < wanted {
		select {
		case at := <-identifies:
			arrived = append(arrived, at)
		case <-deadline:
			t.Fatalf("gateway was identified only %d times in 40s, want %d", len(arrived), wanted)
		}
	}

	firstGap := arrived[1].Sub(arrived[0])
	lastGap := arrived[wanted-1].Sub(arrived[wanted-2])
	if lastGap < 2*firstGap {
		t.Fatalf("reconnect backoff must grow while the session keeps being invalidated: "+
			"gap between IDENTIFY 1 and 2 was %v, between IDENTIFY %d and %d was %v",
			firstGap.Round(10*time.Millisecond), wanted-1, wanted, lastGap.Round(10*time.Millisecond))
	}
}
