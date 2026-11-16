package opus

import (
	"sync"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// closeTogether releases n goroutines into fn at the same instant.
func closeTogether(n int, fn func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			<-start
			fn()
		})
	}
	close(start)
	wg.Wait()
}

// Teardown reaches these from more than one goroutine: disgo closes a receiver
// from its own loop while the conn's removal closes it too, and a provider is
// closed by both the session cleanup and a reconnect that replaces it. A second
// close of the done channel is a panic that takes the whole process down.
func TestCloseIsSafeFromSeveralGoroutines(t *testing.T) {
	for name, newCloser := range map[string]func() func(){
		"VoiceReceiver": func() func() {
			return NewVoiceReceiver(1, nil, telemetry.OpusRecorder{}, NewFanoutHandle()).Close
		},
		"VoiceProvider": func() func() {
			return NewVoiceProvider(make(chan []byte), telemetry.OpusRecorder{}).Close
		},
		"EmptyVoiceProvider": func() func() { return NewEmptyVoiceProvider().Close },
	} {
		t.Run(name, func(t *testing.T) {
			for range 2000 {
				closeTogether(8, newCloser())
			}
		})
	}
}
