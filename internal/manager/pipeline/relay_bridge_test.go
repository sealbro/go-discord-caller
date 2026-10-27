package pipeline

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// Production, 2026-10-27: with no raid running, the idle goroutine profile
// still held one relay bridge (RegisterRelayInputs) per host raid since the
// last restart. The bridge decodes guest audio into the host's mixers and exits
// only when its input channel is closed; the guest pipeline closes its own on
// cleanup, but the host pipelines never do, so every host raid that admits
// guest capture leaks the goroutine, its Opus decoder and the mixers it feeds.
//
// The bridge removes its input from the mixers on the way out, so the input
// still being there after teardown means the bridge is still running.
func TestHostRaidTeardownStopsTheRelayBridge(t *testing.T) {
	for _, mode := range []guild.RaidMode{guild.RaidModeGuildCaller, guild.RaidModeOneManyGuildCaller} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			fx := hostFx()
			p := buildHostParams(t, ctx, fx, mode)
			p.CancelFunc = cancel
			session, start, err := HostFor(mode).Build(ctx, p)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			start()
			t.Cleanup(func() {
				p.OwnerHandle.Close()
				for _, r := range p.Setup.Joined {
					r.Handle.Close()
				}
			})

			hub := mixerOf(t, session.ChannelMixers[fx.ownerChannelID])
			if !slices.Contains(hub.InputIDs(), RelayInputID) {
				t.Fatalf("relay input never registered on the owner channel mixer; got %v", hub.InputIDs())
			}

			session.Cancel()
			session.Cleanup()

			deadline := time.Now().Add(2 * time.Second)
			for slices.Contains(hub.InputIDs(), RelayInputID) {
				if time.Now().After(deadline) {
					t.Fatalf("relay bridge still running 2s after the raid was torn down: " +
						"each host raid leaks its goroutine, decoder and mixers")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
