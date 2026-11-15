package manager

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

type fakeOccupancy struct {
	callers []snowflake.ID
	others  []snowflake.ID // members without the caller role
}

func (f *fakeOccupancy) EnumerateCallers(_, _ snowflake.ID) []snowflake.ID { return f.callers }

func (f *fakeOccupancy) EnumerateUnmutedCallers(_, _ snowflake.ID) []snowflake.ID {
	return f.callers
}

func (f *fakeOccupancy) Listeners(snowflake.ID) []snowflake.ID {
	return append(slices.Clone(f.callers), f.others...)
}

func newIdleWatcher(guildID snowflake.ID, probe occupancyProbe) *sessionPresenceWatcher {
	return &sessionPresenceWatcher{
		guildID:     guildID,
		channels:    []snowflake.ID{900},
		probe:       probe,
		callerRole:  func() snowflake.ID { return 1 },
		stop:        func() {},
		idleTimeout: 10 * time.Millisecond,
	}
}

func TestSessionIdle_StopsWhenOnlyNonCallersAreInChannel(t *testing.T) {
	probe := &fakeOccupancy{}
	var stopped atomic.Bool
	w := newIdleWatcher(7700004, probe)
	w.stop = func() { stopped.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if !stopped.Load() {
		t.Error("session with listeners but no caller must be stopped after the idle timeout")
	}
}

func TestSessionIdle_KeptAliveWhileACallerIsPresent(t *testing.T) {
	probe := &fakeOccupancy{callers: []snowflake.ID{111111}, others: []snowflake.ID{222222}}
	var stopped atomic.Bool
	w := newIdleWatcher(7700005, probe)
	w.stop = func() { stopped.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if stopped.Load() {
		t.Error("session with a caller must not be stopped")
	}
}
