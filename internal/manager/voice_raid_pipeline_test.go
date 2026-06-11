package manager

import (
	"context"
	"reflect"
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

// hostFixture bundles the inputs every host pipeline test needs.
// hostGuildID is fixed; ownerBotID and channel/speaker IDs are configurable.
type hostFixture struct {
	guildID        snowflake.ID
	ownerBotID     snowflake.ID
	ownerChannelID snowflake.ID
	speakerIDs     []snowflake.ID
	speakerChIDs   []snowflake.ID // one per speaker; may repeat (shared channel)
}

// buildHostParams constructs a pipelineParams populated with real objects for
// the given mode. Caller owns ctx cancellation.
func buildHostParams(t *testing.T, ctx context.Context, fx hostFixture, mode guild.RaidMode) pipelineParams {
	t.Helper()
	if len(fx.speakerIDs) != len(fx.speakerChIDs) {
		t.Fatalf("hostFixture: speakerIDs and speakerChIDs length mismatch")
	}

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("pipeline_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, fx.guildID)

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
	allyCode := "TESTCODE"
	allySession := sessions.Create(allyCode, fx.guildID, mode)

	ownerHandle := opus.NewFanoutHandle()
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, audioChanBuf)
	}
	return pipelineParams{
		guildID:      fx.guildID,
		ownerBotID:   fx.ownerBotID,
		cancelFunc:   func() {},
		mode:         mode,
		allyCode:     allyCode,
		allySession:  allySession,
		setup:        setup,
		ownerHandle:  ownerHandle,
		chOwnerOut:   chOwnerOut,
		ownerCleanup: func() {},
		ov:           pool.NewGuildVoice(nil, fx.ownerChannelID),
		gm:           gm,
		allowFilter:  &AllowFilter{},
	}
}

// hostFx is the canonical 2-speaker, distinct-channel layout used across
// pipeline tests. Two speakers in two distinct channels gives mix-minus
// something to exclude.
func hostFx() hostFixture {
	return hostFixture{
		guildID:        snowflake.ID(50),
		ownerBotID:     snowflake.ID(100),
		ownerChannelID: snowflake.ID(1001),
		speakerIDs:     []snowflake.ID{200, 201},
		speakerChIDs:   []snowflake.ID{1002, 1003},
	}
}

func TestPipelineFor_ReturnsExpectedImpl(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode guild.RaidMode
		want string
	}{
		{guild.RaidModeOneCaller, "manager.oneCallerPipeline"},
		{guild.RaidModeGuildCaller, "manager.mixMinusPipeline"},
		{guild.RaidModeOneManyGuildCaller, "manager.starPipeline"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			p := pipelineFor(c.mode)
			gotType := typeName(p)
			if gotType != c.want {
				t.Errorf("pipelineFor(%s): got %s want %s", c.mode, gotType, c.want)
			}
		})
	}
}

// TestOneCallerPipeline_AlwaysOnGraph verifies RaidModeOneCaller now builds
// the full per-speaker-channel + relay mixer graph up-front (started paused)
// and attaches an AutoRouter to the session. Replaces the prior
// TestDirectPipeline_NoChannelMixers which asserted the now-removed bypass.
func TestOneCallerPipeline_AlwaysOnGraph(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeOneCaller)
	session, start, err := pipelineFor(guild.RaidModeOneCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if session.ChannelMixers == nil {
		t.Fatal("OneCaller must now expose ChannelMixers (channel + relay)")
	}
	wantMixers := len(fx.speakerChIDs) + 1 // one per speaker channel + relay
	if got := len(session.ChannelMixers); got != wantMixers {
		t.Errorf("ChannelMixers count: want %d (%d channels + relay), got %d", wantMixers, len(fx.speakerChIDs), got)
	}
	if session.AutoRouter == nil {
		t.Error("OneCaller pipeline must attach AutoRouter to the session")
	}
	if start == nil {
		t.Fatal("build returned nil start func")
	}
	if session.AllyCode != p.allyCode {
		t.Errorf("AllyCode: want %q got %q", p.allyCode, session.AllyCode)
	}
	if session.IsGuest {
		t.Error("host session must not be flagged as guest")
	}
}

// TestMixMinusPipeline_BuildsExpectedTopology verifies that each destination
// channel gets its own mixer and that mix-minus excludes the source channel.
func TestMixMinusPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeGuildCaller)
	session, start, err := pipelineFor(guild.RaidModeGuildCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// (a) ChannelMixers exists for owner channel + each speaker channel.
	wantChIDs := []snowflake.ID{fx.ownerChannelID, fx.speakerChIDs[0], fx.speakerChIDs[1]}
	if got := len(session.ChannelMixers); got != len(wantChIDs) {
		t.Fatalf("ChannelMixers count: want %d got %d", len(wantChIDs), got)
	}
	for _, chID := range wantChIDs {
		if _, ok := session.ChannelMixers[chID]; !ok {
			t.Errorf("missing channel mixer for %s", chID)
		}
	}

	// start() wires fanout inputs to the mixers; cancel ctx on cleanup so
	// the goroutines spawned by startChannelMixers / startRelayBroadcast /
	// DrainWatcher exit promptly.
	start()
	t.Cleanup(func() {
		// Cancel session, then close handles so the install OnClose runs and
		// detaches sources before the goroutines start exiting.
		cancel()
		p.ownerHandle.Close()
		for _, r := range p.setup.joined {
			r.handle.Close()
		}
	})

	// (b)(c) Inspect inputs on each channel mixer.
	// Mix-minus: a channel mixer must NOT contain the source ID from that channel.
	tests := []struct {
		chID            snowflake.ID
		wantInputs      []snowflake.ID
		forbiddenInputs []snowflake.ID
	}{
		// Owner channel mixer: receives speakers (mix-minus excludes owner).
		{fx.ownerChannelID, []snowflake.ID{fx.speakerIDs[0], fx.speakerIDs[1]}, []snowflake.ID{fx.ownerBotID}},
		// Speaker 1's channel mixer: owner + speaker 2 (excludes speaker 1).
		{fx.speakerChIDs[0], []snowflake.ID{fx.ownerBotID, fx.speakerIDs[1]}, []snowflake.ID{fx.speakerIDs[0]}},
		// Speaker 2's channel mixer: owner + speaker 1 (excludes speaker 2).
		{fx.speakerChIDs[1], []snowflake.ID{fx.ownerBotID, fx.speakerIDs[0]}, []snowflake.ID{fx.speakerIDs[1]}},
	}
	for _, tc := range tests {
		mx := mixerOf(t, session.ChannelMixers[tc.chID])
		got := mx.InputIDs()
		for _, want := range tc.wantInputs {
			if !slices.Contains(got, want) {
				t.Errorf("channel %s: missing expected input %s; got %v", tc.chID, want, got)
			}
		}
		for _, forbidden := range tc.forbiddenInputs {
			if slices.Contains(got, forbidden) {
				t.Errorf("channel %s: contains forbidden mix-minus input %s; got %v", tc.chID, forbidden, got)
			}
		}
	}
}

// TestStarPipeline_BuildsExpectedTopology verifies RaidModeOneManyGuildCaller:
// exactly one channel mixer at the owner channel, receiving every speaker as
// input. Speaker channels get raw Opus passthrough via the FanoutHandle, not
// per-channel mixers.
func TestStarPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller)
	session, start, err := pipelineFor(guild.RaidModeOneManyGuildCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Exactly one ChannelMixer entry — the hub at the owner channel.
	if got := len(session.ChannelMixers); got != 1 {
		t.Fatalf("ChannelMixers count: want 1 got %d", got)
	}
	if _, ok := session.ChannelMixers[fx.ownerChannelID]; !ok {
		t.Fatalf("missing hub mixer at owner channel %s", fx.ownerChannelID)
	}

	start()
	t.Cleanup(func() {
		cancel()
		p.ownerHandle.Close()
		for _, r := range p.setup.joined {
			r.handle.Close()
		}
	})

	hubMixer := mixerOf(t, session.ChannelMixers[fx.ownerChannelID])
	got := hubMixer.InputIDs()
	// Hub receives both speakers (star spokes → hub) but NOT the owner.
	for _, want := range fx.speakerIDs {
		if !slices.Contains(got, want) {
			t.Errorf("hub mixer: missing speaker input %s; got %v", want, got)
		}
	}
	if slices.Contains(got, fx.ownerBotID) {
		t.Errorf("hub mixer: must not contain owner self-input; got %v", got)
	}
}

// TestMixMinusPipeline_SharedChannel verifies the buildDestinations dedupe path
// when two speakers occupy the same voice channel — only one mixer is created
// for that channel and it carries both speakers' chOut.
func TestMixMinusPipeline_SharedChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	// Both speakers in the SAME channel.
	fx.speakerChIDs = []snowflake.ID{1002, 1002}

	p := buildHostParams(t, ctx, fx, guild.RaidModeGuildCaller)
	session, start, err := pipelineFor(guild.RaidModeGuildCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// 1 owner mixer + 1 shared speaker-channel mixer = 2.
	if got := len(session.ChannelMixers); got != 2 {
		t.Fatalf("ChannelMixers count (shared channel): want 2 got %d", got)
	}

	start()
	t.Cleanup(func() {
		cancel()
		p.ownerHandle.Close()
		for _, r := range p.setup.joined {
			r.handle.Close()
		}
	})

	// Shared channel mixer should only receive owner (mix-minus excludes the
	// shared channel itself, and iterDeduplicatedCaptures wires only the first
	// speaker as a source).
	sharedMixer := mixerOf(t, session.ChannelMixers[snowflake.ID(1002)])
	got := sharedMixer.InputIDs()
	if !slices.Contains(got, fx.ownerBotID) {
		t.Errorf("shared channel mixer: missing owner input; got %v", got)
	}
	for _, sid := range fx.speakerIDs {
		if slices.Contains(got, sid) {
			t.Errorf("shared channel mixer: must not contain speaker %s (mix-minus); got %v", sid, got)
		}
	}
}

// mixerOf type-asserts a guild.MixerPauser to *opus.Mixer for test inspection.
// All ChannelMixers entries are produced by opus.NewMixer in the pipeline
// builders, so the assertion is guaranteed to succeed in tests.
func mixerOf(t *testing.T, mp guild.MixerPauser) *opus.Mixer {
	t.Helper()
	mx, ok := mp.(*opus.Mixer)
	if !ok {
		t.Fatalf("ChannelMixers entry is not *opus.Mixer; got %T", mp)
	}
	return mx
}

// typeName returns the package-qualified type name of v (excluding pointer prefix).
// Used to assert pipelineFor / guestPipelineFor return the right concrete impl
// without exporting the unexported types.
func typeName(v any) string {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.String()
}
