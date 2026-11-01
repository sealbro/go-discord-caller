package manager

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
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

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

type fakeOccupancy struct {
	listeners []snowflake.ID
	callers   []snowflake.ID
	inChannel map[snowflake.ID]error
	confirmed map[snowflake.ID]bool
}

func (f *fakeOccupancy) HasListeners(snowflake.ID) bool        { return len(f.listeners) > 0 }
func (f *fakeOccupancy) Listeners(snowflake.ID) []snowflake.ID { return f.listeners }
func (f *fakeOccupancy) EnumerateCallers(_, _ snowflake.ID) []snowflake.ID {
	return f.callers
}
func (f *fakeOccupancy) InChannel(_ context.Context, _, user snowflake.ID) (bool, error) {
	return f.confirmed[user], f.inChannel[user]
}

func newReportWatcher(guildID snowflake.ID, probe occupancyProbe) *sessionPresenceWatcher {
	return &sessionPresenceWatcher{
		guildID:     guildID,
		channels:    []snowflake.ID{900},
		probe:       probe,
		callerRole:  func() snowflake.ID { return 1 },
		stop:        func() {},
		idleTimeout: 10 * time.Millisecond,
	}
}

// Who keeps a session alive is reported as counts only: the privacy policy
// promises no member identifier in logs at info level.
func TestOccupancyReport_CountsWithoutUserIDs(t *testing.T) {
	logs := captureLogs(t)

	const guildID = snowflake.ID(7700001)
	probe := &fakeOccupancy{
		listeners: []snowflake.ID{111111, 222222, 333333},
		confirmed: map[snowflake.ID]bool{111111: true},
		inChannel: map[snowflake.ID]error{333333: errors.New("discord unavailable")},
	}

	newReportWatcher(guildID, probe).logOccupants(context.Background())

	got := logs.String()
	for _, want := range []string{"cached=3", "confirmed=1", "unverified=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	for _, id := range []string{"111111", "222222", "333333"} {
		if strings.Contains(got, id) {
			t.Errorf("report leaks user ID %s:\n%s", id, got)
		}
	}
}

func TestOccupancyReport_SkippedWhileACallerIsPresent(t *testing.T) {
	logs := captureLogs(t)

	const guildID = snowflake.ID(7700002)
	probe := &fakeOccupancy{listeners: []snowflake.ID{111111}, callers: []snowflake.ID{111111}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	newReportWatcher(guildID, probe).Run(ctx)

	if got := logs.String(); strings.Contains(got, "keeps the session alive") {
		t.Errorf("a session with a caller must not be reported:\n%s", got)
	}
}

func TestOccupancyReport_SentWhenNoCallerIsPresent(t *testing.T) {
	logs := captureLogs(t)

	const guildID = snowflake.ID(7700003)
	probe := &fakeOccupancy{listeners: []snowflake.ID{111111}, confirmed: map[snowflake.ID]bool{111111: true}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	newReportWatcher(guildID, probe).Run(ctx)

	if got := logs.String(); !strings.Contains(got, "keeps the session alive") {
		t.Errorf("an occupied session without callers must be reported:\n%s", got)
	}
}
