package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// mapHost is a VoiceHost serving fixed voice managers, so a test can tell the
// connections the service opens through the host from the bots' own clients.
type mapHost map[snowflake.ID]voice.Manager

func (h mapHost) VoiceManager(botID snowflake.ID) (voice.Manager, bool) {
	vm, ok := h[botID]
	return vm, ok
}

func TestRaid_JoinsAndLeavesOnlyThroughTheVoiceHost(t *testing.T) {
	for _, side := range raidSides {
		t.Run(side.name, func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			var hostJoins atomic.Int32
			countJoin := func() { hostJoins.Add(1) }
			ownerConn, speakerConn := &fakeVoiceConn{}, &fakeVoiceConn{}
			f.m.voices = mapHost{
				testBotID:         &fakeVoiceManager{conn: ownerConn, onOpen: countJoin},
				roleTestSpeakerID: &fakeVoiceManager{conn: speakerConn, onOpen: countJoin},
			}

			cancel, err := side.start(f)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cancel()
			if err := f.m.StopVoiceRaid(context.Background(), side.guildID); err != nil {
				t.Fatalf("stop: %v", err)
			}

			if got := f.joins.Load(); got != 0 {
				t.Errorf("the bots' own voice managers were joined %d times; every join must go through the VoiceHost", got)
			}
			if got := hostJoins.Load(); got != 2 {
				t.Errorf("VoiceHost joins = %d, want 2 (owner and speaker)", got)
			}
			if ownerConn.closeCount() == 0 {
				t.Error("owner never left through the VoiceHost's connection")
			}
			if speakerConn.closeCount() == 0 {
				t.Error("speaker never left through the VoiceHost's connection")
			}
		})
	}
}

// The owner reaches the drain wait on every owner reconnect. It is not a pool
// bot, so the wait used to find no conn and return at once; serving the owner
// through the VoiceHost must not turn that into a 500 ms poll.
func TestWaitSpeakerConnDrained_DoesNotWaitForTheOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCallerRoleFixture(t)
		boundChannel := roleTestChannels[testGuildID][0]
		f.ownerConn.channelID = boundChannel

		start := time.Now()
		f.m.waitSpeakerConnDrained(testGuildID, testBotID, boundChannel)

		if waited := time.Since(start); waited != 0 {
			t.Errorf("owner reconnect waited %v for a drain only speakers need", waited)
		}
	})
}

func TestLocalVoiceHost_ServesEachBotFromItsOwnClient(t *testing.T) {
	f := newCallerRoleFixture(t)
	host := f.m.voiceHost()

	if vm, ok := host.VoiceManager(testBotID); !ok || vm != f.ownerVM {
		t.Errorf("owner: got (%v, %v), want the owner client's voice manager", vm, ok)
	}
	if vm, ok := host.VoiceManager(roleTestSpeakerID); !ok || vm != f.speakerVM {
		t.Errorf("speaker: got (%v, %v), want the pool client's voice manager", vm, ok)
	}
	if vm, ok := host.VoiceManager(snowflake.ID(799)); ok {
		t.Errorf("unknown bot: got (%v, true), want false", vm)
	}
}
