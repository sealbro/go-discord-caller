package manager

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/telemetry/spantest"
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

// The relay code is the only credential for attaching to someone else's raid,
// so it must stay out of telemetry across a host start, a guest join and both
// stops.
func TestRelayCodeNotInLogsOrSpans(t *testing.T) {
	logs := &lockedBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	ended := spantest.Record()

	f := withCallerRole(newCallerRoleFixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	if code == "" {
		t.Fatal("host raid returned an empty relay code")
	}
	if _, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, code); err != nil {
		t.Fatalf("JoinSession (guest): %v", err)
	}

	if err := f.m.StopVoiceRaid(context.Background(), roleTestGuestGuildID); err != nil {
		t.Fatalf("StopVoiceRaid (guest): %v", err)
	}
	if err := f.m.StopVoiceRaid(context.Background(), testGuildID); err != nil {
		t.Fatalf("StopVoiceRaid (host): %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "guest joined relay session") || !strings.Contains(out, "voice raid started") {
		t.Fatalf("expected the host start and guest join log lines, got:\n%s", out)
	}
	if strings.Contains(out, string(code)) {
		t.Error("a log line carries the relay code")
	}
	recorded := ended()
	if len(recorded) == 0 {
		t.Fatal("no spans recorded")
	}
	for _, span := range recorded {
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.String(), string(code)) {
				t.Errorf("span %q carries the relay code in attribute %q", span.Name(), attr.Key)
			}
		}
	}
}
