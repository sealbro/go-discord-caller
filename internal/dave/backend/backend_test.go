package backend

import (
	"log/slog"
	"testing"

	"github.com/disgoorg/godave"
	"github.com/sealbro/go-discord-caller/internal/dave"
)

type noopCallbacks struct{}

func (noopCallbacks) SendMLSKeyPackage([]byte) error        { return nil }
func (noopCallbacks) SendMLSCommitWelcome([]byte) error     { return nil }
func (noopCallbacks) SendReadyForTransition(uint16) error   { return nil }
func (noopCallbacks) SendInvalidCommitWelcome(uint16) error { return nil }

func TestSessionCreateFuncNeverNil(t *testing.T) {
	// A nil create func would leave disgo without a DAVE session and break
	// voice entirely, so even an out-of-band Impl must return something.
	for _, impl := range []dave.Impl{dave.ImplLibdave, dave.ImplDaveGo, dave.Impl("nonsense")} {
		if SessionCreateFunc(impl, nil) == nil {
			t.Errorf("SessionCreateFunc(%q) = nil", impl)
		}
	}
}

func TestDaveGoSessionsAreTracked(t *testing.T) {
	reg := dave.NewRegistry()
	create := SessionCreateFunc(dave.ImplDaveGo, reg)

	cb := noopCallbacks{}
	sess := create(slog.Default(), "1", cb)
	if sess == nil {
		t.Fatal("create returned nil session")
	}

	// The session must be filed under the callbacks value, because that is the
	// voice.Conn the teardown path has in hand when it calls Release.
	if got := reg.Snapshot().SessionsLive; got != 1 {
		t.Fatalf("SessionsLive = %d, want 1", got)
	}
	reg.Release(cb)
	if got := reg.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("after Release SessionsLive = %d, want 0", got)
	}
}

func TestLibdaveSessionsAreNotTracked(t *testing.T) {
	// golibdave sessions have a no-op Close and expose no counters, so nothing
	// should land in the registry under the default backend.
	reg := dave.NewRegistry()
	create := SessionCreateFunc(dave.ImplLibdave, reg)

	if sess := create(slog.Default(), "1", noopCallbacks{}); sess == nil {
		t.Fatal("create returned nil session")
	}
	if got := reg.Snapshot().SessionsLive; got != 0 {
		t.Fatalf("SessionsLive = %d, want 0", got)
	}
}

func TestNilRegistryTracksNothing(t *testing.T) {
	create := SessionCreateFunc(dave.ImplDaveGo, nil)
	if sess := create(slog.Default(), "1", noopCallbacks{}); sess == nil {
		t.Fatal("create returned nil session")
	}
}

var _ godave.Callbacks = noopCallbacks{}
