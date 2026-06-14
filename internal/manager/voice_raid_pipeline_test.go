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
		// Force every channel into routeMix so the router-driven pipelines'
		// initial Recompute attaches mixer inputs immediately. Topology tests
		// then assert against the post-install state instead of having to
		// simulate caller joins. C=2 also exercises the §1 row-2 cascade
		// for both source-baseline and multi-source cases.
		voiceProbe: stubCallerCounter(2),
	}
}

// stubCallerCounter is a CallerEnumerator that synthesises N caller IDs for
// any channel — pipeline tests use this to drive the router into a predictable
// mode. The synthesised IDs are derived from the channel ID so each call
// returns a stable set the user-set comparison can recognise across re-routes.
type stubCallerCounter int

func (s stubCallerCounter) EnumerateCallers(channelID, _ snowflake.ID) []snowflake.ID {
	out := make([]snowflake.ID, int(s))
	for i := range out {
		out[i] = snowflake.ID(uint64(channelID)*1000 + uint64(i+1))
	}
	return out
}

// HasListeners returns true unconditionally so pipeline tests can assert on
// the routed (unpaused) mixer state without setting up a separate listener
// fixture for every channel.
func (s stubCallerCounter) HasListeners(_ snowflake.ID) bool { return true }

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
		{guild.RaidModeGuildCaller, "manager.guildCallerPipeline"},
		{guild.RaidModeOneManyGuildCaller, "manager.starCallerPipeline"},
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

// TestGuildCallerPipeline_BuildsExpectedTopology verifies that each
// destination channel gets its own mixer, the relay mixer is exposed too, and
// mix-minus excludes the source channel from each channel mixer.
// (Replaces TestMixMinusPipeline_BuildsExpectedTopology — the pipeline now
// runs through the router and exposes the relay mixer via ChannelMixers under
// the synthetic relayDestID.)
func TestGuildCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeGuildCaller)
	session, start, err := pipelineFor(guild.RaidModeGuildCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// (a) ChannelMixers exists for owner channel + each speaker channel + relay.
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
		t.Error("GuildCaller pipeline must attach AutoRouter to the session")
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

	// (b) Mixer inputs are now synth IDs (one per user, per source feeding
	// the destination) rather than source bot IDs. With stubCallerCounter(2)
	// each of the 3 source channels has 2 enumerated users. Mix-minus says
	// each channel mixer is fed by 2 source channels (N-1) × 2 users = 4
	// synth inputs. AllowGuestCapture also registers the relay input (id 1).
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
			t.Errorf("channel %s: want %d synth inputs (mix-minus excludes own source), got %d in %v", chID, wantSynthInputs, synthCount, got)
		}
		if !slices.Contains(got, relayInputID) {
			t.Errorf("channel %s: missing relay input (id=%d) from registerRelayInputs; got %v", chID, relayInputID, got)
		}
	}
}

// TestStarCallerPipeline_BuildsExpectedTopology verifies RaidModeOneManyGuildCaller:
// the hub mixer at owner channel receives every speaker as input, and the
// relay mixer is exposed alongside it via ChannelMixers (under the synthetic
// relayDestID). Speaker channels still get raw Opus passthrough from the
// owner via OpusTargets — they have no dedicated per-channel mixer.
func TestStarCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller)
	session, start, err := pipelineFor(guild.RaidModeOneManyGuildCaller).build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Hub mixer + relay mixer = 2 entries (speakers have no per-channel mixer).
	if got := len(session.ChannelMixers); got != 2 {
		t.Fatalf("ChannelMixers count: want 2 (hub + relay) got %d", got)
	}
	if _, ok := session.ChannelMixers[fx.ownerChannelID]; !ok {
		t.Fatalf("missing hub mixer at owner channel %s", fx.ownerChannelID)
	}
	if _, ok := session.ChannelMixers[relayDestID]; !ok {
		t.Fatalf("missing relay mixer under relayDestID")
	}
	if session.AutoRouter == nil {
		t.Error("star pipeline must attach AutoRouter to the session")
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
	// Hub is fed only by speaker sources (star spokes → hub). Per-user
	// keying produces (2 speakers) × (2 users per stub) = 4 synth inputs;
	// AllowGuestCapture adds the relay input (id 1) too.
	synthCount := 0
	for _, id := range got {
		if uint64(id)>>63 == 1 {
			synthCount++
		}
	}
	if synthCount != 4 {
		t.Errorf("hub mixer: want 4 synth inputs (2 speakers × 2 users), got %d in %v", synthCount, got)
	}
	if !slices.Contains(got, relayInputID) {
		t.Errorf("hub mixer: missing relay input (id=%d); got %v", relayInputID, got)
	}
}

// TestGuildCallerPipeline_SharedChannel verifies the buildDestinations dedupe
// path when two speakers occupy the same voice channel — only one mixer is
// created for that channel and it carries both speakers' chOut. The relay
// mixer brings the count to 3.
func TestGuildCallerPipeline_SharedChannel(t *testing.T) {
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

	// 1 owner mixer + 1 shared speaker-channel mixer + relay = 3.
	if got := len(session.ChannelMixers); got != 3 {
		t.Fatalf("ChannelMixers count (shared channel): want 3 got %d", got)
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
	// speaker as a source). Owner channel has 2 stub users, so the shared
	// channel mixer should have exactly 2 synth inputs (one per user) + the
	// relay input from AllowGuestCapture.
	sharedMixer := mixerOf(t, session.ChannelMixers[snowflake.ID(1002)])
	got := sharedMixer.InputIDs()
	synthCount := 0
	for _, id := range got {
		if uint64(id)>>63 == 1 {
			synthCount++
		}
	}
	if synthCount != 2 {
		t.Errorf("shared channel mixer: want 2 synth inputs (owner × 2 users), got %d in %v", synthCount, got)
	}
	if !slices.Contains(got, relayInputID) {
		t.Errorf("shared channel mixer: missing relay input (id=%d); got %v", relayInputID, got)
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
