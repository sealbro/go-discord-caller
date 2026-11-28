package pipeline

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// A listener guest's mix is fed only by the relay, so nothing local can tell it
// the host is about to talk again: a lull on the host side must not leave it
// paused, or the host's next words are dropped until the drain watcher notices.
func TestListenerGuestHearsHostRightAfterALull(t *testing.T) {
	synctest.Test(t, testListenerGuestHearsHostRightAfterALull)
}

func testListenerGuestHearsHostRightAfterALull(t *testing.T) {
	const frame = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeGuildCaller, guild.RaidModeAllyListener)
	out := listenerOuts(&p)

	_, start, cleanup, err := GuestListenerPipeline{}.Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	start()
	t.Cleanup(func() {
		p.AllySession.RemoveGuild(fx.guestGuildID)
		cleanup()
	})
	played := countPackets(ctx, out)

	const frames = 50
	host := tonePackets(t, 140, frames+1)
	for i := range frames {
		time.Sleep(frame)
		p.AllySession.BroadcastFromGuild(fx.hostGuildID, opus.CopyOpusFrame(host[i]))
		synctest.Wait()
	}
	time.Sleep(5 * frame)
	synctest.Wait()
	if played.Load() == 0 {
		t.Fatal("speaker was handed nothing while the host talked")
	}

	time.Sleep(2 * opus.DrainIdleTimeout)
	synctest.Wait()
	beforeResume := played.Load()

	p.AllySession.BroadcastFromGuild(fx.hostGuildID, opus.CopyOpusFrame(host[frames]))
	time.Sleep(5 * frame)
	synctest.Wait()
	if played.Load() == beforeResume {
		t.Errorf("the host's first frame after a %v lull did not reach the speaker within %v", 2*opus.DrainIdleTimeout, 5*frame)
	}
}
