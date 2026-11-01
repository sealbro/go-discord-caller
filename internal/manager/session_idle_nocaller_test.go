package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

func TestSessionIdle_StopsWhenOnlyNonCallersAreInChannel(t *testing.T) {
	probe := &fakeOccupancy{listeners: []snowflake.ID{111111}}
	var stopped atomic.Bool
	w := newReportWatcher(7700004, probe)
	w.stop = func() { stopped.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if !stopped.Load() {
		t.Error("session with listeners but no caller must be stopped after the idle timeout")
	}
}

func TestSessionIdle_KeptAliveWhileACallerIsPresent(t *testing.T) {
	probe := &fakeOccupancy{listeners: []snowflake.ID{111111}, callers: []snowflake.ID{111111}}
	var stopped atomic.Bool
	w := newReportWatcher(7700005, probe)
	w.stop = func() { stopped.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if stopped.Load() {
		t.Error("session with a caller must not be stopped")
	}
}
