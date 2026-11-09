//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestE5g_RelayCodeNotInLogsOrSpans checks that the relay code, the only
// credential for joining a host's raid, stays out of telemetry: it is the
// value a guild needs to attach to someone else's session.
func TestE5g_RelayCodeNotInLogsOrSpans(t *testing.T) {
	guestPrereqs(t)

	logs := &lockedBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		_ = provider.Shutdown(context.Background())
	})

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)
	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	code, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyListener, code); err != nil {
		t.Fatalf("JoinSession (guest): %v", err)
	}

	// Spans end and the last log lines are written only when the raids stop.
	if err := mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID); err != nil {
		t.Fatalf("StopVoiceRaid (guest): %v", err)
	}
	if err := mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID); err != nil {
		t.Fatalf("StopVoiceRaid (host): %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "guest joined relay session") || !strings.Contains(out, "voice raid started") {
		t.Fatalf("expected the host start and guest join log lines, got:\n%s", out)
	}
	if strings.Contains(out, code) {
		t.Error("a log line carries the relay code")
	}
	ended := spans.Ended()
	if len(ended) == 0 {
		t.Fatal("no spans recorded")
	}
	for _, span := range ended {
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.Emit(), code) {
				t.Errorf("span %q carries the relay code in attribute %q", span.Name(), attr.Key)
			}
		}
	}
}
