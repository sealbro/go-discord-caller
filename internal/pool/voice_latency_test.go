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

		var once sync.Once
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

	const (
		guildID   = snowflake.ID(1430511050704289835)
		botID     = snowflake.ID(1529403555667251310)
		channelID = snowflake.ID(1430511051383767101)
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
