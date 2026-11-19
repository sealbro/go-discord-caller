package pool

import (
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// joinTraces marks each step of a voice handshake on the span of the Join
// waiting for it. disgo's Open reports nothing until the handshake completes
// and its voice logs are off in production, so a join that times out would
// otherwise not say whether Discord never assigned a voice server, the voice
// gateway never became ready, or UDP discovery failed.
type joinTraces struct {
	mu    sync.Mutex
	spans map[joinKey]trace.Span
}

type joinKey struct{ botID, guildID snowflake.ID }

var defaultJoinTraces = &joinTraces{spans: make(map[joinKey]trace.Span)}

// begin records handshake steps for botID in guildID on span until the
// returned func is called.
func (t *joinTraces) begin(botID, guildID snowflake.ID, span trace.Span) func() {
	if botID == 0 || !span.IsRecording() {
		return func() {}
	}
	key := joinKey{botID, guildID}
	t.mu.Lock()
	t.spans[key] = span
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		if t.spans[key] == span {
			delete(t.spans, key)
		}
		t.mu.Unlock()
	}
}

func (t *joinTraces) event(botID, guildID snowflake.ID, name string, attrs ...attribute.KeyValue) {
	t.mu.Lock()
	span := t.spans[joinKey{botID, guildID}]
	t.mu.Unlock()
	if span != nil {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

func (t *joinTraces) voiceOp(botID, guildID snowflake.ID, op voice.Opcode) {
	switch op {
	case voice.OpcodeHello:
		t.event(botID, guildID, "voice.gateway.hello")
	case voice.OpcodeReady:
		t.event(botID, guildID, "voice.gateway.ready")
	case voice.OpcodeSessionDescription:
		t.event(botID, guildID, "voice.session.description")
	}
}

// VoiceJoinTraceListeners feeds the main-gateway half of botID's voice
// handshake to joinTraces; the voice-gateway half comes from newConn. Every
// client this process builds should register them.
func VoiceJoinTraceListeners(botID snowflake.ID) []bot.EventListener {
	return []bot.EventListener{
		bot.NewListenerFunc(func(e *events.GuildVoiceStateUpdate) {
			if e.VoiceState.UserID != botID {
				return
			}
			defaultJoinTraces.event(botID, e.VoiceState.GuildID, "voice.state.update",
				attribute.Bool("voice.channel.joined", e.VoiceState.ChannelID != nil))
		}),
		bot.NewListenerFunc(func(e *events.VoiceServerUpdate) {
			attrs := []attribute.KeyValue{attribute.Bool("voice.endpoint.assigned", e.Endpoint != nil)}
			if e.Endpoint != nil {
				attrs = append(attrs, attribute.String("voice.region", telemetry.ParseVoiceRegion(*e.Endpoint)))
			}
			defaultJoinTraces.event(botID, e.GuildID, "voice.server.update", attrs...)
		}),
	}
}
