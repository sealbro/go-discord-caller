package manager

import (
	"context"
	"slices"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// guestFixture is the guest-side counterpart of hostFixture. The host's session
// is created separately so the guest can attach via ally.Manager.
type guestFixture struct {
	hostGuildID    snowflake.ID
	guestGuildID   snowflake.ID
	ownerBotID     snowflake.ID
	ownerChannelID snowflake.ID
	speakerIDs     []snowflake.ID
	speakerChIDs   []snowflake.ID
}

func guestFx() guestFixture {
	return guestFixture{
		hostGuildID:    snowflake.ID(50),
		guestGuildID:   snowflake.ID(60),
		ownerBotID:     snowflake.ID(100),
		ownerChannelID: snowflake.ID(2001),
		speakerIDs:     []snowflake.ID{300, 301},
		speakerChIDs:   []snowflake.ID{2002, 2003},
	}
}

// buildGuestParams constructs a guestPipelineParams populated with real objects.
func buildGuestParams(t *testing.T, ctx context.Context, fx guestFixture, hostMode, guestMode guild.RaidMode) guestPipelineParams {
	t.Helper()
	if len(fx.speakerIDs) != len(fx.speakerChIDs) {
		t.Fatalf("guestFixture: speakerIDs and speakerChIDs length mismatch")
	}

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("guest_pipeline_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, fx.guestGuildID)

	joined := make([]speakerResult, 0, len(fx.speakerIDs))
	for i, sid := range fx.speakerIDs {
		joined = append(joined, speakerResult{
			speaker: guild.Speaker{ID: sid, Enabled: true},
			chOut:   make(chan []byte, audioChanBuf),
			handle:  opus.NewFanoutHandle(),
			gv:      pool.NewGuildVoice(nil, fx.speakerChIDs[i]),
			cleanup: func() {},
		})
	}
	outs := make([]chan<- []byte, 0, len(joined))
	speakers := make([]guild.Speaker, 0, len(joined))
	for _, r := range joined {
		outs = append(outs, r.chOut)
		speakers = append(speakers, r.speaker)
	}
	setup := &raidSetup{
		joined:         joined,
		speakers:       speakers,
		speakerCleanup: func() {},
		outs:           outs,
	}

	sessions := ally.NewManager()
	allyCode := "GUESTCDE"
	// Create host session first, then have guest join.
	sessions.Create(allyCode, fx.hostGuildID, hostMode)
	allySession, err := sessions.Join(allyCode, fx.guestGuildID)
	if err != nil {
		t.Fatalf("ally.Manager.Join: %v", err)
	}

	// Owner relay enabled only when the guest captures — matches the production
	// JoinSession flow where ownerHandle is the gate for "include the guest
	// owner as a fanout source".
	var ownerChOut chan []byte
	var ownerHandle *opus.FanoutHandle
	if guestMode.WithCapture() {
		ownerChOut = make(chan []byte, audioChanBuf)
		ownerHandle = opus.NewFanoutHandle()
	}

	return guestPipelineParams{
		guestGuildID:   fx.guestGuildID,
		ownerBotID:     fx.ownerBotID,
		ownerChannelID: fx.ownerChannelID,
		cancelFunc:     func() {},
		code:           allyCode,
		guestMode:      guestMode,
		allySession:    allySession,
		setup:          setup,
		ownerChOut:     ownerChOut,
		ownerHandle:    ownerHandle,
		guestGm:        gm,
		allowFilter:    &AllowFilter{},
		// Drive the router into mix mode so topology assertions can observe
		// the post-install state without simulating voice events.
		voiceProbe: stubCallerCounter(2),
	}
}

func TestGuestPipelineFor_ReturnsExpectedImpl(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode guild.RaidMode
		want string
	}{
		{guild.RaidModeAllyListener, "manager.guestListenerPipeline"},
		{guild.RaidModeAllyCaller, "manager.guestCallerPipeline"},
		{guild.RaidModeOneManyAllyCaller, "manager.guestStarCallerPipeline"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			p := guestPipelineFor(c.mode)
			if gotType := typeName(p); gotType != c.want {
				t.Errorf("guestPipelineFor(%s): got %s want %s", c.mode, gotType, c.want)
			}
		})
	}
}

// TestGuestListenerPipeline_NoChannelMixers verifies RaidModeAllyListener is
// pure relay — no local channel mixers are created.
func TestGuestListenerPipeline_NoChannelMixers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeOneCaller, guild.RaidModeAllyListener)

	session, start, cleanup, err := guestPipelineFor(guild.RaidModeAllyListener).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if session.ChannelMixers != nil {
		t.Errorf("Listener mode must not create ChannelMixers; got %d entries", len(session.ChannelMixers))
	}
	if !session.IsGuest {
		t.Error("guest session must have IsGuest=true")
	}
	if start == nil || cleanup == nil {
		t.Fatal("build returned nil start or cleanup")
	}
	// start() only calls AddGuild on ally.Session — no goroutines spawned.
	start()
	cleanup()
}

// TestGuestCallerPipeline_BuildsExpectedTopology verifies full mix-minus on
// the guest side: a channel mixer per destination, mix-minus excluding the
// channel's own source.
func TestGuestCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeGuildCaller, guild.RaidModeAllyCaller)
	session, start, cleanup, err := guestPipelineFor(guild.RaidModeAllyCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Expect a mixer for each speaker channel + the owner relay channel +
	// the relay mixer (router-driven build always exposes it).
	wantChIDs := []snowflake.ID{fx.ownerChannelID, fx.speakerChIDs[0], fx.speakerChIDs[1], relayDestID}
	if got := len(session.ChannelMixers); got != len(wantChIDs) {
		t.Fatalf("ChannelMixers count: want %d got %d", len(wantChIDs), got)
	}
	for _, chID := range wantChIDs {
		if _, ok := session.ChannelMixers[chID]; !ok {
			t.Errorf("missing channel mixer for %s", chID)
		}
	}
	if session.AutoRouter == nil {
		t.Error("guest caller pipeline must attach AutoRouter to the session")
	}

	start()
	t.Cleanup(func() {
		cancel()
		cleanup()
		for _, r := range p.setup.joined {
			r.handle.Close()
		}
		if p.ownerHandle != nil {
			p.ownerHandle.Close()
		}
	})

	// Per-user keying: each non-own source contributes 2 synth inputs (stub
	// reports 2 callers per channel). 3 sources total minus the own source
	// → 2 sources × 2 users = 4 synth inputs per channel mixer. relayInputID
	// is added by registerRelayInputs so the host's broadcast reaches guest
	// speakers.
	const wantSynthInputs = 4
	for _, chID := range []snowflake.ID{fx.ownerChannelID, fx.speakerChIDs[0], fx.speakerChIDs[1]} {
		mx := mixerOf(t, session.ChannelMixers[chID])
		got := mx.InputIDs()
		synthCount := 0
		for _, id := range got {
			if uint64(id)>>63 == 1 {
				synthCount++
			}
		}
		if synthCount != wantSynthInputs {
			t.Errorf("channel %s: want %d synth inputs (mix-minus), got %d in %v", chID, wantSynthInputs, synthCount, got)
		}
		if !slices.Contains(got, relayInputID) {
			t.Errorf("channel %s: missing relay input %d; got %v", chID, relayInputID, got)
		}
	}
}

// TestGuestStarCallerPipeline_BuildsExpectedTopology verifies guest star mode
// (RaidModeOneManyAllyCaller): no local channel mixers; speakers contribute
// only to the relay; channels receive audio via the host relay (registered as
// relay inputs on per-channel mixers — but star mode places everything on the
// relay, not channel mixers).
//
// Per voice_raid_guest.go:guestStarCallerPipeline: it creates a relayMixer and
// no channel mixers. start() calls wireFanoutOneMany with ownerChannelID=0, so
// all sources go to relay only. The guest's destination channels receive audio
// via allySession.AddGuild → broadcast → speaker chOut directly, not via local
// mixers.
func TestGuestStarCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller, guild.RaidModeOneManyAllyCaller)
	session, start, cleanup, err := guestPipelineFor(guild.RaidModeOneManyAllyCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Star guest exposes only the relay mixer via ChannelMixers (the local
	// destination channels are fed by the host's broadcast through AddGuild,
	// not by any local mixer).
	if got := len(session.ChannelMixers); got != 1 {
		t.Fatalf("star guest ChannelMixers count: want 1 (relay only), got %d", got)
	}
	if _, ok := session.ChannelMixers[relayDestID]; !ok {
		t.Fatalf("star guest must expose the relay mixer under relayDestID")
	}
	if !session.IsGuest {
		t.Error("guest session must have IsGuest=true")
	}
	if session.AutoRouter == nil {
		t.Error("star guest must attach AutoRouter to the session")
	}

	start()
	t.Cleanup(func() {
		cancel()
		cleanup()
		for _, r := range p.setup.joined {
			r.handle.Close()
		}
		if p.ownerHandle != nil {
			p.ownerHandle.Close()
		}
	})
}
