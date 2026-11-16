package pipeline

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
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

// A /stop can land between committing a raid and running its start func, so
// the cleanup may run first. The bridge must then never start, or it leaks
// with nothing left to close it.
func TestRelayBridgeNeverStartsAfterEarlyTeardown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fx := hostFx()
	mode := guild.RaidModeGuildCaller
	p := buildHostParams(t, ctx, fx, mode)
	p.CancelFunc = cancel
	session, start, err := HostFor(mode).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		p.OwnerHandle.Close()
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
		}
	})

	session.Cleanup()
	start()

	hub := mixerOf(t, session.ChannelMixers[fx.ownerChannelID])
	if slices.Contains(hub.InputIDs(), RelayInputID) {
		t.Fatalf("relay bridge started after the raid was torn down; nothing will ever stop it")
	}
}

func TestRelayInputsNotRegisteredWhenDecoderFails(t *testing.T) {
	const (
		hostGuildID  = snowflake.ID(50)
		guestGuildID = snowflake.ID(60)
		channelID    = snowflake.ID(2001)
	)
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("relay_bridge_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(t.Context(), guestGuildID)
	mx, err := opus.NewMixer(gm.Opus)
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	session := ally.NewManager().Create("TESTCODE", hostGuildID, guild.RaidModeGuildCaller)
	failingDecoder := func(int, int) (*hraban.Decoder, error) { return nil, errors.New("decoder unavailable") }

	got := registerRelayInputs(gm, session, []*DestChannel{{ChannelID: channelID}},
		map[snowflake.ID]*opus.Mixer{channelID: mx}, failingDecoder)

	if got != nil {
		t.Errorf("returned %d relay inputs, want none when the decoder cannot be created", len(got))
	}
	if slices.Contains(session.GuestGuildIDs(), guestGuildID) {
		t.Error("guild stayed registered in the session with nothing consuming its relay input")
	}
	deadline := time.Now().Add(time.Second)
	for slices.Contains(mx.InputIDs(), RelayInputID) {
		if time.Now().After(deadline) {
			t.Fatal("relay input left on the channel mixer after the decoder failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
