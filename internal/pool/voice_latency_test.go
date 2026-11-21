package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// helloSession is the DAVE session for a gateway that receives Hello, which
// makes disgo identify and ask the session which protocol version it supports.
type helloSession struct{ epochSession }

func (*helloSession) MaxSupportedProtocolVersion() int { return 0 }

func voiceLatencyPoints(t *testing.T, reader *sdkmetric.ManualReader) []float64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var points []float64
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.voice.server.latency" {
				continue
			}
			gauge, ok := md.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("gdc.voice.server.latency is %T, want Gauge[float64]", md.Data)
			}
			for _, dp := range gauge.DataPoints {
				points = append(points, dp.Value)
			}
		}
	}
	return points
}

// ackingVoiceServer readies the gateway, then answers each heartbeat only once
// ack is closed, so a test controls when the first real round trip completes.
// firstHeartbeat is closed when the first heartbeat arrives.
func ackingVoiceServer(t *testing.T, heartbeatInterval time.Duration, firstHeartbeat chan<- struct{}, ack <-chan struct{}) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{}
	var once sync.Once
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if err := conn.WriteJSON(map[string]any{
			"op": voice.OpcodeHello,
			"d":  map[string]any{"heartbeat_interval": heartbeatInterval.Milliseconds()},
		}); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"op": voice.OpcodeResumed}); err != nil {
			return
		}

		for {
			var msg struct {
				Op voice.Opcode `json:"op"`
				D  struct {
					T int64 `json:"t"`
				} `json:"d"`
			}
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if json.Unmarshal(raw, &msg) != nil || msg.Op != voice.OpcodeHeartbeat {
				continue
			}
			once.Do(func() { close(firstHeartbeat) })
			select {
			case <-ack:
			case <-r.Context().Done():
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"op": voice.OpcodeHeartbeatACK,
				"d":  map[string]any{"t": msg.D.T},
			}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// latencyMetrics reports botID's voice latency into a fresh reader, with the
// guild's region known.
func latencyMetrics(t *testing.T, guildID, botID snowflake.ID) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("pool_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	_ = VoiceRegionListeners(botID, &m.Voice)
	m.Voice.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")
	return reader
}

func openLatencyConn(t *testing.T, srv *httptest.Server, guildID, botID, channelID snowflake.ID) voice.Conn {
	t.Helper()
	vm := newVoiceManager(botID, sessionOf(&helloSession{}), withDialer(trustingDialer(srv)))
	return openVoiceConn(t, vm, srv, guildID, botID, channelID)
}

// disgo's voice gateway reports lastHeartbeatReceived - lastHeartbeatSent, and
// starts the heartbeat loop by setting lastHeartbeatReceived to now while
// lastHeartbeatSent still holds the dial time. Until the first ACK, Latency is
// therefore a small positive number — the handshake duration — not the negative
// one it looks like, and it must not be exported as a round trip.
func TestVoiceLatencyNotExportedBeforeFirstAck(t *testing.T) {
	t.Parallel()

	// Unique in the package: regionMetrics is keyed by bot ID, so a parallel
	// test reusing one would retire this test's series.
	const (
		guildID   = snowflake.ID(901)
		botID     = snowflake.ID(902)
		channelID = snowflake.ID(903)
		rtt       = 40 * time.Millisecond
	)

	reader := latencyMetrics(t, guildID, botID)
	firstHeartbeat := make(chan struct{})
	ack := make(chan struct{})
	releaseAck := sync.OnceFunc(func() { close(ack) })
	// The reading must be taken before the second heartbeat, which is ACKed at
	// once, replaces it.
	srv := ackingVoiceServer(t, 500*time.Millisecond, firstHeartbeat, ack)
	t.Cleanup(releaseAck)

	t.Cleanup(openLatencyConn(t, srv, guildID, botID, channelID).Gateway().Close)

	heartbeatSent := false
	for deadline := time.Now().Add(5 * time.Second); !heartbeatSent; {
		if got := voiceLatencyPoints(t, reader); len(got) != 0 {
			t.Fatalf("exported %v ms before the first heartbeat ACK, want nothing", got)
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway never sent a heartbeat")
		}
		select {
		case <-firstHeartbeat:
			heartbeatSent = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(rtt)
	if got := voiceLatencyPoints(t, reader); len(got) != 0 {
		t.Fatalf("exported %v ms while the first heartbeat waited for its ACK, want nothing", got)
	}
	releaseAck()

	var got []float64
	if !eventually(2*time.Second, func() bool {
		got = voiceLatencyPoints(t, reader)
		return len(got) == 1 && got[0] >= float64(rtt.Milliseconds()) && got[0] < 1000
	}) {
		t.Fatalf("after the ACK: got %v ms, want one reading of about %d ms", got, rtt.Milliseconds())
	}
}

// A heartbeat that stops being ACKed must stop being reported: otherwise a dying
// connection keeps exporting its last healthy round trip.
func TestAckLatencyGoesStaleWhenAcksStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			interval = 10 * time.Millisecond
			rtt      = 30 * time.Millisecond
		)
		var a ackLatency
		a.observe(voice.OpcodeHello, voice.GatewayMessageDataHello{HeartbeatInterval: float64(interval.Milliseconds())})
		a.observe(voice.OpcodeHeartbeatACK, voice.GatewayMessageDataHeartbeatACK{T: time.Now().Add(-rtt).UnixMilli()})
		if got := a.read(); got != rtt {
			t.Fatalf("right after an ACK: got %v, want %v", got, rtt)
		}

		time.Sleep(staleAfterHeartbeats * interval)
		if got := a.read(); got != rtt {
			t.Fatalf("%d heartbeat intervals after the last ACK: got %v, want %v still", staleAfterHeartbeats, got, rtt)
		}
		time.Sleep(time.Nanosecond)
		if got := a.read(); got != 0 {
			t.Fatalf("over %d heartbeat intervals after the last ACK: got %v, want nothing", staleAfterHeartbeats, got)
		}
	})
}

// Two conns can briefly exist for one bot and guild — the replacement is built
// before the old one's teardown runs. The old conn's teardown must not retire
// the series the new conn reports.
func TestOldConnTeardownKeepsNewConnLatency(t *testing.T) {
	t.Parallel()

	// The test must end before a heartbeat follows an ACK: disgo v0.20.0
	// writes the heartbeat times from its listen goroutine unsynchronised, and
	// the race detector flags the next heartbeat's read.
	const (
		guildID   = snowflake.ID(911)
		botID     = snowflake.ID(912)
		channelID = snowflake.ID(913)
		heartbeat = 300 * time.Millisecond
	)

	reader := latencyMetrics(t, guildID, botID)
	ack := make(chan struct{})
	close(ack)
	srv := ackingVoiceServer(t, heartbeat, make(chan struct{}), ack)

	oldConn := openLatencyConn(t, srv, guildID, botID, channelID)
	newConn := openLatencyConn(t, srv, guildID, botID, channelID)
	t.Cleanup(newConn.Gateway().Close)

	if !eventually(3*time.Second, func() bool { return len(voiceLatencyPoints(t, reader)) != 0 }) {
		t.Fatal("the new conn never reported a latency")
	}

	oldConn.Close(context.Background())

	for end := time.Now().Add(heartbeat / 3); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if got := voiceLatencyPoints(t, reader); len(got) == 0 {
			t.Fatal("the old conn's teardown retired the new conn's latency series")
		}
	}
}
