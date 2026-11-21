package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// startHostRaidAsync starts a raid in the background and returns what it
// reports.
func startHostRaidAsync(t *testing.T, f *callerRoleFixture) <-chan error {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan error, 1)
	go func() {
		_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
		started <- err
	}()
	return started
}

// holdOwnerJoin parks the start at the owner's CreateConn — past the speaker
// joins, short of the commit — until release is closed.
func holdOwnerJoin(f *callerRoleFixture) (reached <-chan struct{}, release chan<- struct{}) {
	in, out := make(chan struct{}), make(chan struct{})
	f.ownerVM.onOpen = func() {
		close(in)
		<-out
	}
	return in, out
}

// A raid is only committed once every bot is in, so for the whole startup —
// seconds, a Discord round trip per bot — /stop finds nothing in m.statuses and
// reports that no raid is running. The raid then comes up anyway, bots and all,
// and the manager who asked for it to stop has to ask again once it has
// finished. Stopping a raid that is still starting must abort it.
func TestStopVoiceRaid_AbortsAStartStillInFlight(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reached, release := holdOwnerJoin(f)

	started := startHostRaidAsync(t, f)
	await(t, reached, "the start to reach the owner join")

	stopErr := f.m.StopVoiceRaid(context.Background(), testGuildID)
	close(release)

	if err := await(t, started, "the start to return"); err == nil {
		t.Error("a start that was stopped must not report success")
	}
	if errors.Is(stopErr, ErrNoActiveSession) {
		t.Error("/stop reported no active raid while one was starting, and the raid came up anyway")
	} else if stopErr != nil {
		t.Errorf("stop failed for an unrelated reason: %v", stopErr)
	}

	f.m.mu.RLock()
	committed := f.m.statuses[testGuildID].Session
	f.m.mu.RUnlock()
	if committed != nil {
		t.Error("a stopped start must not commit its session")
	}
	if _, ok := f.m.sessions.GetByGuild(testGuildID); ok {
		t.Error("a stopped start must not leave its ally session registered")
	}
	if n := f.ownerConn.closeCount(); n == 0 {
		t.Error("a stopped start must leave the owner bot's voice channel")
	}
	if n := f.speakerConn.closeCount(); n == 0 {
		t.Error("a stopped start must leave the speaker bot's voice channel")
	}
}

// A /stop that lands while the speakers are still joining fails every join with
// the start's own cancellation. Reporting that as "no speakers joined: verify
// speaker channels are bound…" sends whoever reads it to check a setup that is
// fine; the start was cancelled, and the error has to say so.
func TestStopDuringSpeakerJoins_ReportsCancellationNotMissingSpeakers(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))

	// The speaker's handshake hangs, as disgo's Open does until its context ends.
	joining := make(chan struct{})
	f.speakerConn.open = func(ctx context.Context) error {
		close(joining)
		<-ctx.Done()
		return ctx.Err()
	}

	started := startHostRaidAsync(t, f)
	await(t, joining, "the start to reach the speaker join")
	if err := f.m.StopVoiceRaid(context.Background(), testGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}

	err := await(t, started, "the start to return")
	if errors.Is(err, ErrNoSpeakers) {
		t.Errorf("a start stopped during the speaker joins reported %q", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("start error = %v, want it to wrap context.Canceled", err)
	}
}

// A raid only becomes active once every bot is in, and for the seconds before
// that /setup let the caller role, the channels and the relay code change under
// the start: a speaker that later reconnected went to the newly bound channel,
// not the one the raid was wired for. The settings stay locked from the moment
// a start begins until its teardown is over.
func TestSetupLocked_WhileARaidIsStillStarting(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	reached, release := holdOwnerJoin(f)

	if f.m.SetupLocked(testGuildID) {
		t.Fatal("settings locked before any raid")
	}

	started := startHostRaidAsync(t, f)
	await(t, reached, "the start to reach the owner join")
	locked := f.m.SetupLocked(testGuildID)
	close(release)
	if err := await(t, started, "the start to return"); err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	if !locked {
		t.Error("settings were unlocked while the raid was still starting")
	}

	if !f.m.SetupLocked(testGuildID) {
		t.Error("settings unlocked while the raid is active")
	}
	if err := f.m.StopVoiceRaid(context.Background(), testGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	if f.m.SetupLocked(testGuildID) {
		t.Error("settings still locked after the raid stopped")
	}
}
