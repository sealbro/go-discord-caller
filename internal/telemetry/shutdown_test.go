package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestShutdownAllGivesEachProviderItsOwnBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	var lastCtxErr error
	stalled := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	last := func(ctx context.Context) error {
		lastCtxErr = ctx.Err()
		return nil
	}

	shutdownAll(budget, stalled, last)

	if lastCtxErr != nil {
		t.Errorf("provider after a stalled one started with an expired context: %v", lastCtxErr)
	}
}
