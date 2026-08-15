package dave

import (
	"testing"

	"github.com/disgoorg/godave"
	davego "github.com/thomas-vilte/dave-go/session"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Impl
		wantErr bool
	}{
		{in: "", want: ImplLibdave},
		{in: "libdave", want: ImplLibdave},
		{in: "golibdave", want: ImplLibdave},
		{in: "godave", want: ImplLibdave},
		{in: "LIBDAVE", want: ImplLibdave},
		{in: "dave-go", want: ImplDaveGo},
		{in: "davego", want: ImplDaveGo},
		{in: "dave_go", want: ImplDaveGo},
		{in: "  Dave-Go  ", want: ImplDaveGo},
		// Unknown values still yield a usable Impl alongside the error, so a
		// caller that only warns keeps a working voice stack.
		{in: "mls", want: Default, wantErr: true},
	}

	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("Parse(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// noopCallbacks satisfies godave.Callbacks for sessions built in tests; nothing
// is ever sent because no protocol event is driven.
type noopCallbacks struct{ id int }

func (noopCallbacks) SendMLSKeyPackage([]byte) error      { return nil }
func (noopCallbacks) SendMLSCommitWelcome([]byte) error   { return nil }
func (noopCallbacks) SendReadyForTransition(uint16) error { return nil }
func (noopCallbacks) SendInvalidCommitWelcome(uint16) error {
	return nil
}

func newTestSession(cb godave.Callbacks) *davego.Session {
	return davego.New("1", cb)
}

func TestRegistryTracksAndReleases(t *testing.T) {
	r := NewRegistry()
	key := noopCallbacks{id: 1}

	if got := r.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("empty registry SessionsLive = %d, want 0", got)
	}

	r.Track(key, newTestSession(key))
	if got := r.Snapshot().SessionsLive; got != 1 {
		t.Fatalf("after Track SessionsLive = %d, want 1", got)
	}

	r.Release(key)
	if got := r.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("after Release SessionsLive = %d, want 0", got)
	}

	// Releasing an unknown key (libdave backend, or a second Close of the same
	// connection) must not panic or disturb the totals.
	r.Release(noopCallbacks{id: 99})
	r.Release(key)
	if got := r.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("after redundant Release SessionsLive = %d, want 0", got)
	}
}

func TestRegistryTrackReplacesSameKey(t *testing.T) {
	r := NewRegistry()
	key := noopCallbacks{id: 1}

	r.Track(key, newTestSession(key))
	r.Track(key, newTestSession(key))

	// The first session is retired rather than leaked, so exactly one stays live.
	if got := r.Snapshot().SessionsLive; got != 1 {
		t.Fatalf("SessionsLive = %d, want 1", got)
	}
	if n := len(r.live); n != 1 {
		t.Fatalf("live sessions = %d, want 1", n)
	}
}

func TestRegistryCloseAll(t *testing.T) {
	r := NewRegistry()
	for i := range 3 {
		key := noopCallbacks{id: i}
		r.Track(key, newTestSession(key))
	}
	if got := r.Snapshot().SessionsLive; got != 3 {
		t.Fatalf("SessionsLive = %d, want 3", got)
	}

	r.CloseAll()
	if got := r.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("after CloseAll SessionsLive = %d, want 0", got)
	}
	r.CloseAll() // idempotent
}

func TestNilRegistryIsUsable(t *testing.T) {
	// The libdave path never builds a registry, and GuildVoice may hold a nil
	// SessionCloser — every method has to tolerate that.
	var r *Registry
	key := noopCallbacks{id: 1}
	r.Track(key, newTestSession(key))
	r.Release(key)
	r.CloseAll()
	if got := r.Snapshot(); got.SessionsLive != 0 {
		t.Fatalf("nil registry Snapshot = %+v, want zero value", got)
	}
	if err := r.StartMetrics(nil); err != nil {
		t.Fatalf("nil registry StartMetrics: %v", err)
	}
}
