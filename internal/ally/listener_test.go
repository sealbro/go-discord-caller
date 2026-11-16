package ally

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// A listener's queue is drained by one speaker bot into one audio stream, so
// it can carry one stream: the host's. A capturing guest's packets interleaved
// into it make every listener there hear both garbled.
func TestSession_ListenerHearsOnlyTheHost(t *testing.T) {
	t.Parallel()
	const listenerID, otherGuestID = snowflake.ID(3), snowflake.ID(4)
	s := newTestSession(t)
	queue := make(chan []byte, 4)
	s.AddListener(listenerID, []chan<- []byte{queue})

	s.BroadcastFromGuild(hostID, []byte{1})
	s.BroadcastFromGuild(otherGuestID, []byte{2})

	if len(queue) != 1 {
		t.Fatalf("listener queue holds %d packets, want only the host's", len(queue))
	}
	if got := <-queue; got[0] != 1 {
		t.Errorf("listener received packet %v, want the host's", got)
	}
}
