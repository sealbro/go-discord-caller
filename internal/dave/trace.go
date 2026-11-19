package dave

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// traceLen bounds a session's trace: a handshake and a few transitions fit,
// and a session lives as long as its voice connection.
const traceLen = 32

// Trace is the recent DAVE signalling a session went through, oldest first. It
// carries no user IDs: membership is recorded as a count.
type Trace []TraceEvent

// TraceEvent is one DAVE signalling step, with the backend's readiness and the
// number of other clients in the channel right after it.
type TraceEvent struct {
	At         time.Duration // since the session was created
	Event      string
	Transition uint16
	Epoch      int
	Version    uint16
	Ready      bool
	Others     int
}

func (e TraceEvent) String() string {
	var b strings.Builder
	b.WriteString(e.Event)
	switch e.Event {
	case "prepare_epoch":
		fmt.Fprintf(&b, " e%d v%d", e.Epoch, e.Version)
	case "prepare_transition":
		fmt.Fprintf(&b, " t%d v%d", e.Transition, e.Version)
	case "execute_transition", "welcome", "commit":
		fmt.Fprintf(&b, " t%d", e.Transition)
	case "ack":
		fmt.Fprintf(&b, " v%d", e.Version)
	}
	fmt.Fprintf(&b, " ready=%t others=%d", e.Ready, e.Others)
	return b.String()
}

// LogValue renders the trace as one line per event, prefixed by its offset.
func (t Trace) LogValue() slog.Value {
	lines := make([]string, len(t))
	for i, e := range t {
		lines[i] = fmt.Sprintf("+%s %s", e.At.Round(time.Millisecond), e)
	}
	return slog.StringValue(strings.Join(lines, "; "))
}
