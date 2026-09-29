package manager

import (
	"testing"

	"github.com/sealbro/go-discord-caller/internal/pool"
)

// A reconnect promises a single retry after a short backoff. A failed join does
// not cost only the handshake timeout: pool.GuildVoice.Join releases the
// half-open conn afterwards, and that Leave blocks until disgo gives up on a
// conn Discord never answered for. With both attempts sharing one budget, that
// budget has to cover the whole sequence — otherwise the first failure leaves
// it already expired, the backoff select takes the cancelled branch, and the
// retry never runs.
func TestReconnectBudgetSurvivesAFailedJoin(t *testing.T) {
	t.Parallel()

	if reconnectBudget <= pool.VoiceJoinCost {
		t.Fatalf("reconnect budget %v is spent by one failed join (%v): the retry never runs", reconnectBudget, pool.VoiceJoinCost)
	}
	if want := 2*pool.VoiceJoinCost + reconnectBackoff; reconnectBudget < want {
		t.Errorf("reconnect budget = %v, want at least %v for two attempts plus the %v backoff", reconnectBudget, want, reconnectBackoff)
	}
}
