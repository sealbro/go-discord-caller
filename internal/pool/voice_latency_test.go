package pool

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
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
		guildID   = snowflake.ID(1530000000000000101)
		botID     = snowflake.ID(1530000000000000102)
		channelID = snowflake.ID(1530000000000000103)
		rtt       = 40 * time.Millisecond
	)

	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("pool_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	_ = VoiceRegionListeners(botID, &m.Voice)
	m.Voice.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")

	firstHeartbeat := make(chan struct{})
	ack := make(chan struct{})
	var ackOnce sync.Once
	releaseAck := func() { ackOnce.Do(func() { close(ack) }) }
	srv := ackingVoiceServer(t, time.Second, firstHeartbeat, ack)
	t.Cleanup(releaseAck)

	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		botID,
		voice.WithDaveSessionCreateFunc(func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
			return &helloSession{}
		}),
		SafeAudioSenderOpt(),
		VoiceConnOpt(),
		voice.WithConnConfigOpts(voice.WithConnGatewayConfigOpts(voice.WithGatewayDialer(&websocket.Dialer{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		}))),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	conn := vm.CreateConn(guildID)
	gw := conn.Gateway()
	t.Cleanup(gw.Close)

	openCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gw.Open(openCtx, voice.State{
		GuildID:   guildID,
		UserID:    botID,
		ChannelID: channelID,
		SessionID: "session",
		Token:     "token",
		Endpoint:  strings.TrimPrefix(srv.URL, "https://"),
	}); err != nil {
		t.Fatalf("open voice gateway: %v", err)
	}

	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := voiceLatencyPoints(t, reader); len(got) != 0 {
			t.Fatalf("exported %v ms before the first heartbeat ACK, want nothing", got)
		}
	}

	select {
	case <-firstHeartbeat:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never sent a heartbeat")
	}
	time.Sleep(rtt)
	releaseAck()

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := voiceLatencyPoints(t, reader)
		if len(got) == 1 && got[0] >= float64(rtt.Milliseconds()) && got[0] < 1000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the ACK: got %v ms, want one reading of about %d ms", got, rtt.Milliseconds())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A heartbeat that stops being ACKed must stop being reported: otherwise a dying
// connection keeps exporting its last healthy round trip.
func TestAckLatencyGoesStaleWhenAcksStop(t *testing.T) {
	t.Parallel()

	var a ackLatency
	a.observe(voice.OpcodeHello, voice.GatewayMessageDataHello{HeartbeatInterval: 10})
	a.observe(voice.OpcodeHeartbeatACK, voice.GatewayMessageDataHeartbeatACK{T: time.Now().Add(-30 * time.Millisecond).UnixMilli()})
	if a.read() <= 0 {
		t.Fatalf("right after an ACK: got %v, want a reading", a.read())
	}

	time.Sleep(50 * time.Millisecond)
	if got := a.read(); got != 0 {
		t.Fatalf("two heartbeat intervals after the last ACK: got %v, want nothing", got)
	}
}

func openLatencyConn(t *testing.T, srv *httptest.Server, guildID, botID, channelID snowflake.ID) voice.Conn {
	t.Helper()

	vm := voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		botID,
		voice.WithDaveSessionCreateFunc(func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session {
			return &helloSession{}
		}),
		SafeAudioSenderOpt(),
		VoiceConnOpt(),
		voice.WithConnConfigOpts(voice.WithConnGatewayConfigOpts(voice.WithGatewayDialer(&websocket.Dialer{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		}))),
		voice.WithLogger(slog.New(slog.DiscardHandler)),
	)
	conn := vm.CreateConn(guildID)

	openCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Gateway().Open(openCtx, voice.State{
		GuildID:   guildID,
		UserID:    botID,
		ChannelID: channelID,
		SessionID: "session",
		Token:     "token",
		Endpoint:  strings.TrimPrefix(srv.URL, "https://"),
	}); err != nil {
		t.Fatalf("open voice gateway: %v", err)
	}
	return conn
}

// Two conns can briefly exist for one bot and guild — the replacement is built
// before the old one's teardown runs. The old conn's teardown must not retire
// the series the new conn reports.
func TestOldConnTeardownKeepsNewConnLatency(t *testing.T) {
	t.Parallel()

	const (
		guildID   = snowflake.ID(1530000000000000201)
		botID     = snowflake.ID(1530000000000000202)
		channelID = snowflake.ID(1530000000000000203)
	)

	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("pool_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	_ = VoiceRegionListeners(botID, &m.Voice)
	m.Voice.SetRegion(guildID, botID, "c-fra12-50a8bfa.discord.media:443")

	ack := make(chan struct{})
	close(ack)
	srv := ackingVoiceServer(t, 500*time.Millisecond, make(chan struct{}), ack)

	oldConn := openLatencyConn(t, srv, guildID, botID, channelID)
	newConn := openLatencyConn(t, srv, guildID, botID, channelID)
	t.Cleanup(newConn.Gateway().Close)

	deadline := time.Now().Add(3 * time.Second)
	for len(voiceLatencyPoints(t, reader)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the new conn never reported a latency")
		}
		time.Sleep(10 * time.Millisecond)
	}

	oldConn.Close(context.Background())

	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if got := voiceLatencyPoints(t, reader); len(got) == 0 {
			t.Fatal("the old conn's teardown retired the new conn's latency series")
		}
	}
}
