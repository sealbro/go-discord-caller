package ally

import (
	"sync"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// A host stopping its raid and the registry replacing a stale session can both
// close the same session.
func TestSessionCloseIsSafeFromSeveralGoroutines(t *testing.T) {
	for range 2000 {
		s := newSession("ABCDEFGH", 1, guild.RaidModeOneCaller)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				<-start
				s.close()
			})
		}
		close(start)
		wg.Wait()
	}
}
