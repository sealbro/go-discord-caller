// Package gatewaytest is a fake Discord gateway for tests of how a client
// behaves when its connection keeps failing.
package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

const (
	heartbeatIntervalMillis = 41250
	invalidateAfter         = 500 * time.Millisecond
	identifiesWanted        = 4
	identifiesDeadline      = 40 * time.Second
	requiredGrowth          = 2
)

type frame struct {
	Op int             `json:"op"`
	T  string          `json:"t,omitempty"`
	S  int             `json:"s,omitempty"`
	D  json.RawMessage `json:"d"`
}

// IdentifyingGateway accepts every IDENTIFY with READY and then invalidates the
// session, so a client that reconnects at once is identified over and over. It
// returns the gateway URL and a channel receiving each IDENTIFY's arrival time.
func IdentifyingGateway(t testing.TB) (wsURL string, identifies <-chan time.Time) {
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

		send := func(f frame) bool { return conn.WriteJSON(f) == nil }
		if !send(frame{Op: opHello, D: json.RawMessage(fmt.Sprintf(`{"heartbeat_interval":%d}`, heartbeatIntervalMillis))}) {
			return
		}
		for {
			var in frame
			if err := conn.ReadJSON(&in); err != nil {
				return
			}
			switch in.Op {
			case opHeartbeat:
				if !send(frame{Op: opHeartbeatAck}) {
					return
				}
			case opIdentify:
				arrivals <- time.Now()
				ready := `{"v":10,"user":{"id":"1","username":"bot","bot":true},"guilds":[],` +
					`"session_id":"s","resume_gateway_url":"` + strings.Replace(srv.URL, "http", "ws", 1) + `",` +
					`"application":{"id":"1","flags":0}}`
				if !send(frame{Op: opDispatch, T: "READY", S: 1, D: json.RawMessage(ready)}) {
					return
				}
				time.Sleep(invalidateAfter)
				if !send(frame{Op: opInvalidSession, D: json.RawMessage(`false`)}) {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http", "ws", 1), arrivals
}

// RequireGrowingGaps fails unless the gaps between consecutive IDENTIFYs grow:
// the last gap of the first few must be at least twice the first.
func RequireGrowingGaps(t testing.TB, identifies <-chan time.Time) {
	t.Helper()
	var arrived []time.Time
	deadline := time.After(identifiesDeadline)
	for len(arrived) < identifiesWanted {
		select {
		case at := <-identifies:
			arrived = append(arrived, at)
		case <-deadline:
			t.Fatalf("gateway was identified only %d times in %v, want %d", len(arrived), identifiesDeadline, identifiesWanted)
		}
	}

	firstGap := arrived[1].Sub(arrived[0])
	lastGap := arrived[identifiesWanted-1].Sub(arrived[identifiesWanted-2])
	if lastGap < requiredGrowth*firstGap {
		t.Fatalf("reconnect backoff must grow while the session keeps being invalidated: "+
			"gap between IDENTIFY 1 and 2 was %v, between IDENTIFY %d and %d was %v",
			firstGap.Round(10*time.Millisecond), identifiesWanted-1, identifiesWanted, lastGap.Round(10*time.Millisecond))
	}
}
