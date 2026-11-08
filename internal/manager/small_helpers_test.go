package manager

import (
	"errors"
	"testing"
)

func TestChannelTakenErrorMatchesSentinel(t *testing.T) {
	t.Parallel()
	err := error(&ChannelTakenError{BotID: 701})
	if !errors.Is(err, ErrChannelTaken) {
		t.Error("ChannelTakenError must match ErrChannelTaken")
	}
	if err.Error() != ErrChannelTaken.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), ErrChannelTaken.Error())
	}
}

func TestCountSpeakers(t *testing.T) {
	t.Parallel()
	if got := countSpeakers(3, false); got != 3 {
		t.Errorf("without owner = %d, want 3", got)
	}
	if got := countSpeakers(3, true); got != 4 {
		t.Errorf("with owner = %d, want 4", got)
	}
}
