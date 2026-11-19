package pool

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// scriptedConn runs handshake during Open, then behaves like disgo's connImpl:
// Open returns once the handshake completes, or blocks until ctx is done.
type scriptedConn struct {
	voice.Conn
	handshake func() (completed bool)
}

func (c *scriptedConn) Open(ctx context.Context, _ snowflake.ID, _ bool, _ bool) error {
	if c.handshake() {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *scriptedConn) Close(context.Context) {}

type scriptedVoiceManager struct {
	hangingVoiceManager
	conn *scriptedConn
}

func (m *scriptedVoiceManager) CreateConn(snowflake.ID) voice.Conn { return m.conn }
func (m *scriptedVoiceManager) GetConn(snowflake.ID) voice.Conn    { return m.conn }

func dispatch(listeners []bot.EventListener, e bot.Event) {
	for _, l := range listeners {
		l.OnEvent(e)
	}
}

func voiceStateUpdate(botID, guildID snowflake.ID, channelID *snowflake.ID) *events.GuildVoiceStateUpdate {
	return &events.GuildVoiceStateUpdate{GenericGuildVoiceState: &events.GenericGuildVoiceState{
		VoiceState: discord.VoiceState{GuildID: guildID, UserID: botID, ChannelID: channelID},
	}}
}

func voiceServerUpdate(guildID snowflake.ID, endpoint *string) *events.VoiceServerUpdate {
	return &events.VoiceServerUpdate{EventVoiceServerUpdate: gateway.EventVoiceServerUpdate{GuildID: guildID, Endpoint: endpoint}}
}

// joinWithSpan runs Join inside a recorded span, the way the raid code wraps it
// in voice.conn.open, and returns the span's events.
func joinWithSpan(t *testing.T, gv GuildVoice, guildID snowflake.ID) ([]sdktrace.Event, error) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)).Tracer("pool_test")

	ctx, span := tracer.Start(context.Background(), "voice.conn.open")
	_, err := gv.Join(ctx, guildID)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	return ended[0].Events(), err
}

func eventNames(evs []sdktrace.Event) []string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = e.Name
	}
	return names
}

func eventAttr(evs []sdktrace.Event, name string, key attribute.Key) (attribute.Value, bool) {
	for _, e := range evs {
		if e.Name != name {
			continue
		}
		for _, a := range e.Attributes {
			if a.Key == key {
				return a.Value, true
			}
		}
	}
	return attribute.Value{}, false
}

func TestJoinSpanRecordsEachHandshakeStep(t *testing.T) {
	const (
		botID     = snowflake.ID(701)
		guildID   = snowflake.ID(702)
		channelID = snowflake.ID(703)
	)
	listeners := VoiceJoinTraceListeners(botID)
	endpoint := "c-waw11-0a1b2c3d.discord.media:443"
	ch := channelID

	conn := &scriptedConn{handshake: func() bool {
		dispatch(listeners, voiceStateUpdate(botID, guildID, &ch))
		dispatch(listeners, voiceServerUpdate(guildID, &endpoint))
		for _, op := range []voice.Opcode{voice.OpcodeHello, voice.OpcodeReady, voice.OpcodeSessionDescription} {
			defaultJoinTraces.voiceOp(botID, guildID, op)
		}
		return true
	}}
	gv := NewGuildVoice(&scriptedVoiceManager{conn: conn}, channelID).ForBot(botID)

	evs, err := joinWithSpan(t, gv, guildID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	want := []string{"voice.state.update", "voice.server.update", "voice.gateway.hello", "voice.gateway.ready", "voice.session.description"}
	if got := eventNames(evs); !slices.Equal(got, want) {
		t.Fatalf("span events = %v, want %v", got, want)
	}
	if v, _ := eventAttr(evs, "voice.server.update", "voice.region"); v.AsString() != "c-waw" {
		t.Errorf("voice.region = %q, want c-waw", v.AsString())
	}
	if v, _ := eventAttr(evs, "voice.state.update", "voice.channel.joined"); !v.AsBool() {
		t.Error("voice.channel.joined = false, want true")
	}
}

// The case the events exist for: Discord answers the join but never assigns a
// voice server, so the handshake hangs until Join's own timeout.
func TestJoinSpanShowsWhereAHungHandshakeStopped(t *testing.T) {
	const (
		botID     = snowflake.ID(711)
		otherBot  = snowflake.ID(712)
		guildID   = snowflake.ID(713)
		channelID = snowflake.ID(714)
	)
	listeners := VoiceJoinTraceListeners(botID)
	ch := channelID

	conn := &scriptedConn{handshake: func() bool {
		dispatch(listeners, voiceStateUpdate(otherBot, guildID, &ch))
		dispatch(listeners, voiceStateUpdate(botID, guildID, &ch))
		dispatch(listeners, voiceServerUpdate(guildID, nil))
		return false
	}}
	gv := NewGuildVoice(&scriptedVoiceManager{conn: conn}, channelID).ForBot(botID)
	gv.joinTimeout = 50 * time.Millisecond
	gv.retryCleanupTimeout = 50 * time.Millisecond
	gv.retryTimeout = 50 * time.Millisecond
	gv.cleanupTimeout = 50 * time.Millisecond

	evs, err := joinWithSpan(t, gv, guildID)
	if err == nil {
		t.Fatal("Join succeeded on a handshake that never completed")
	}

	want := []string{"voice.state.update", "voice.server.update", "voice.join.retry", "voice.state.update", "voice.server.update"}
	if got := eventNames(evs); !slices.Equal(got, want) {
		t.Fatalf("span events = %v, want %v", got, want)
	}
	if v, _ := eventAttr(evs, "voice.server.update", "voice.endpoint.assigned"); v.AsBool() {
		t.Error("voice.endpoint.assigned = true for a null endpoint")
	}
	if _, ok := eventAttr(evs, "voice.server.update", "voice.region"); ok {
		t.Error("voice.region recorded without an endpoint")
	}

	// A step that lands after Join gave up belongs to no join.
	defaultJoinTraces.voiceOp(botID, guildID, voice.OpcodeReady)
	defaultJoinTraces.mu.Lock()
	_, still := defaultJoinTraces.spans[joinKey{botID, guildID}]
	defaultJoinTraces.mu.Unlock()
	if still {
		t.Error("join trace still registered after Join returned")
	}
}
