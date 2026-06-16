package pipeline

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

// guestFixture is the guest-side counterpart of hostFixture.
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

// buildGuestParams constructs GuestParams populated with real objects.
func buildGuestParams(t *testing.T, ctx context.Context, fx guestFixture, hostMode, guestMode guild.RaidMode) GuestParams {
	t.Helper()
	if len(fx.speakerIDs) != len(fx.speakerChIDs) {
		t.Fatalf("guestFixture: speakerIDs and speakerChIDs length mismatch")
	}

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("guest_pipeline_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, fx.guestGuildID)

	joined := make([]SpeakerResult, 0, len(fx.speakerIDs))
	for i, sid := range fx.speakerIDs {
		joined = append(joined, SpeakerResult{
			Speaker: guild.Speaker{ID: sid, Enabled: true},
			ChOut:   make(chan []byte, opus.AudioChanBuf),
			Handle:  opus.NewFanoutHandle(),
			GV:      pool.NewGuildVoice(nil, fx.speakerChIDs[i]),
			Cleanup: func() {},
		})
	}
	outs := make([]chan<- []byte, 0, len(joined))
	speakers := make([]guild.Speaker, 0, len(joined))
	for _, r := range joined {
		outs = append(outs, r.ChOut)
		speakers = append(speakers, r.Speaker)
	}
	setup := &Setup{
		Joined:         joined,
		Speakers:       speakers,
		SpeakerCleanup: func() {},
		Outs:           outs,
	}

	sessions := ally.NewManager()
	allyCode := ally.Code("GUESTCDE")
	sessions.Create(allyCode, fx.hostGuildID, hostMode)
	allySession, err := sessions.Join(allyCode, fx.guestGuildID)
	if err != nil {
		t.Fatalf("ally.Manager.Join: %v", err)
	}

	var ownerChOut chan []byte
	var ownerHandle *opus.FanoutHandle
	if guestMode.WithCapture() {
		ownerChOut = make(chan []byte, opus.AudioChanBuf)
		ownerHandle = opus.NewFanoutHandle()
	}

	return GuestParams{
		GuestGuildID:   fx.guestGuildID,
		OwnerBotID:     fx.ownerBotID,
		OwnerChannelID: fx.ownerChannelID,
		CancelFunc:     func() {},
		Code:           allyCode,
		GuestMode:      guestMode,
		AllySession:    allySession,
		Setup:          setup,
		OwnerChOut:     ownerChOut,
		OwnerHandle:    ownerHandle,
		GuestGM:        gm,
		AllowFilter:    stubAllowFilter{},
		VoiceProbe:     stubCallerCounter(2),
	}
}

func TestGuestFor_ReturnsExpectedImpl(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode guild.RaidMode
		want string
	}{
		{guild.RaidModeAllyListener, "pipeline.GuestListenerPipeline"},
		{guild.RaidModeAllyCaller, "pipeline.GuestCallerPipeline"},
		{guild.RaidModeOneManyAllyCaller, "pipeline.GuestStarCallerPipeline"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			p := GuestFor(c.mode)
			if gotType := typeName(p); gotType != c.want {
				t.Errorf("GuestFor(%s): got %s want %s", c.mode, gotType, c.want)
			}
		})
	}
}

func TestGuestListenerPipeline_NoChannelMixers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeOneCaller, guild.RaidModeAllyListener)

	session, start, cleanup, err := GuestFor(guild.RaidModeAllyListener).Build(ctx, p)
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
	start()
	cleanup()
}

func TestGuestCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeGuildCaller, guild.RaidModeAllyCaller)
	session, start, cleanup, err := GuestFor(guild.RaidModeAllyCaller).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	wantChIDs := []snowflake.ID{fx.ownerChannelID, fx.speakerChIDs[0], fx.speakerChIDs[1], RelayDestID}
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
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
		}
		if p.OwnerHandle != nil {
			p.OwnerHandle.Close()
		}
	})

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
		if !slices.Contains(got, RelayInputID) {
			t.Errorf("channel %s: missing relay input %d; got %v", chID, RelayInputID, got)
		}
	}
}

func TestGuestStarCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller, guild.RaidModeOneManyAllyCaller)
	session, start, cleanup, err := GuestFor(guild.RaidModeOneManyAllyCaller).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := len(session.ChannelMixers); got != 1 {
		t.Fatalf("star guest ChannelMixers count: want 1 (relay only), got %d", got)
	}
	if _, ok := session.ChannelMixers[RelayDestID]; !ok {
		t.Fatalf("star guest must expose the relay mixer under RelayDestID")
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
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
		}
		if p.OwnerHandle != nil {
			p.OwnerHandle.Close()
		}
	})
}
