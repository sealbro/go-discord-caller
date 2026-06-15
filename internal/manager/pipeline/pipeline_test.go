package pipeline

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

// stubAllowFilter is a no-op guild.AllowUpdater for tests that don't exercise
// allow updates. Pipeline tests never call Set; they only need the interface
// for guild.Session.AllowFilter assignment.
type stubAllowFilter struct{}

func (stubAllowFilter) Set(snowflake.ID, bool) {}

// hostFixture bundles the inputs every host pipeline test needs.
type hostFixture struct {
	guildID        snowflake.ID
	ownerBotID     snowflake.ID
	ownerChannelID snowflake.ID
	speakerIDs     []snowflake.ID
	speakerChIDs   []snowflake.ID // one per speaker; may repeat (shared channel)
}

// buildHostParams constructs Params populated with real objects for mode.
func buildHostParams(t *testing.T, ctx context.Context, fx hostFixture, mode guild.RaidMode) Params {
	t.Helper()
	if len(fx.speakerIDs) != len(fx.speakerChIDs) {
		t.Fatalf("hostFixture: speakerIDs and speakerChIDs length mismatch")
	}

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("pipeline_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, fx.guildID)

	joined := make([]SpeakerResult, 0, len(fx.speakerIDs))
	for i, sid := range fx.speakerIDs {
		joined = append(joined, SpeakerResult{
			Speaker: guild.Speaker{ID: sid, Enabled: true},
			ChOut:   make(chan []byte, audioChanBuf),
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
	allyCode := ally.Code("TESTCODE")
	allySession := sessions.Create(allyCode, fx.guildID, mode)

	ownerHandle := opus.NewFanoutHandle()
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, audioChanBuf)
	}
	return Params{
		GuildID:      fx.guildID,
		OwnerBotID:   fx.ownerBotID,
		CancelFunc:   func() {},
		Mode:         mode,
		AllyCode:     allyCode,
		AllySession:  allySession,
		Setup:        setup,
		OwnerHandle:  ownerHandle,
		ChOwnerOut:   chOwnerOut,
		OwnerCleanup: func() {},
		OV:           pool.NewGuildVoice(nil, fx.ownerChannelID),
		GM:           gm,
		RoleID:       0,
		AllowFilter:  stubAllowFilter{},
		// Force every channel into RouteMix so the router-driven pipelines'
		// initial Recompute attaches mixer inputs immediately.
		VoiceProbe: stubCallerCounter(2),
	}
}

// stubCallerCounter is a CallerEnumerator that synthesises N caller IDs for
// any channel; pipeline tests use it to drive the router into a predictable
// mode. IDs are derived from the channel ID so each call returns a stable set.
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
// pipeline tests.
func hostFx() hostFixture {
	return hostFixture{
		guildID:        snowflake.ID(50),
		ownerBotID:     snowflake.ID(100),
		ownerChannelID: snowflake.ID(1001),
		speakerIDs:     []snowflake.ID{200, 201},
		speakerChIDs:   []snowflake.ID{1002, 1003},
	}
}

func TestHostFor_ReturnsExpectedImpl(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode guild.RaidMode
		want string
	}{
		{guild.RaidModeOneCaller, "pipeline.OneCallerPipeline"},
		{guild.RaidModeGuildCaller, "pipeline.GuildCallerPipeline"},
		{guild.RaidModeOneManyGuildCaller, "pipeline.StarCallerPipeline"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			p := HostFor(c.mode)
			gotType := typeName(p)
			if gotType != c.want {
				t.Errorf("HostFor(%s): got %s want %s", c.mode, gotType, c.want)
			}
		})
	}
}

// TestOneCallerPipeline_AlwaysOnGraph verifies RaidModeOneCaller now builds
// the full per-speaker-channel + relay mixer graph up-front (started paused)
// and attaches an AutoRouter to the session.
func TestOneCallerPipeline_AlwaysOnGraph(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeOneCaller)
	session, start, err := HostFor(guild.RaidModeOneCaller).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if session.ChannelMixers == nil {
		t.Fatal("OneCaller must now expose ChannelMixers (channel + relay)")
	}
	wantMixers := len(fx.speakerChIDs) + 1
	if got := len(session.ChannelMixers); got != wantMixers {
		t.Errorf("ChannelMixers count: want %d (%d channels + relay), got %d", wantMixers, len(fx.speakerChIDs), got)
	}
	if session.AutoRouter == nil {
		t.Error("OneCaller pipeline must attach AutoRouter to the session")
	}
	if start == nil {
		t.Fatal("build returned nil start func")
	}
	if session.AllyCode != p.AllyCode {
		t.Errorf("AllyCode: want %q got %q", p.AllyCode, session.AllyCode)
	}
	if session.IsGuest {
		t.Error("host session must not be flagged as guest")
	}
}

// TestGuildCallerPipeline_BuildsExpectedTopology verifies that each
// destination channel gets its own mixer, the relay mixer is exposed too, and
// mix-minus excludes the source channel from each channel mixer.
func TestGuildCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeGuildCaller)
	session, start, err := HostFor(guild.RaidModeGuildCaller).Build(ctx, p)
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
		t.Error("GuildCaller pipeline must attach AutoRouter to the session")
	}

	start()
	t.Cleanup(func() {
		cancel()
		p.OwnerHandle.Close()
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
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
			t.Errorf("channel %s: want %d synth inputs (mix-minus excludes own source), got %d in %v", chID, wantSynthInputs, synthCount, got)
		}
		if !slices.Contains(got, RelayInputID) {
			t.Errorf("channel %s: missing relay input (id=%d) from RegisterRelayInputs; got %v", chID, RelayInputID, got)
		}
	}
}

// TestStarCallerPipeline_BuildsExpectedTopology verifies RaidModeOneManyGuildCaller:
// the hub mixer at owner channel receives every speaker as input, and the
// relay mixer is exposed alongside it via ChannelMixers (under RelayDestID).
func TestStarCallerPipeline_BuildsExpectedTopology(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	p := buildHostParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller)
	session, start, err := HostFor(guild.RaidModeOneManyGuildCaller).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := len(session.ChannelMixers); got != 2 {
		t.Fatalf("ChannelMixers count: want 2 (hub + relay) got %d", got)
	}
	if _, ok := session.ChannelMixers[fx.ownerChannelID]; !ok {
		t.Fatalf("missing hub mixer at owner channel %s", fx.ownerChannelID)
	}
	if _, ok := session.ChannelMixers[RelayDestID]; !ok {
		t.Fatalf("missing relay mixer under RelayDestID")
	}
	if session.AutoRouter == nil {
		t.Error("star pipeline must attach AutoRouter to the session")
	}

	start()
	t.Cleanup(func() {
		cancel()
		p.OwnerHandle.Close()
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
		}
	})

	hubMixer := mixerOf(t, session.ChannelMixers[fx.ownerChannelID])
	got := hubMixer.InputIDs()
	synthCount := 0
	for _, id := range got {
		if uint64(id)>>63 == 1 {
			synthCount++
		}
	}
	if synthCount != 4 {
		t.Errorf("hub mixer: want 4 synth inputs (2 speakers × 2 users), got %d in %v", synthCount, got)
	}
	if !slices.Contains(got, RelayInputID) {
		t.Errorf("hub mixer: missing relay input (id=%d); got %v", RelayInputID, got)
	}
}

// TestGuildCallerPipeline_SharedChannel verifies the BuildDestinations dedupe
// path when two speakers occupy the same voice channel.
func TestGuildCallerPipeline_SharedChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := hostFx()
	fx.speakerChIDs = []snowflake.ID{1002, 1002}

	p := buildHostParams(t, ctx, fx, guild.RaidModeGuildCaller)
	session, start, err := HostFor(guild.RaidModeGuildCaller).Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := len(session.ChannelMixers); got != 3 {
		t.Fatalf("ChannelMixers count (shared channel): want 3 got %d", got)
	}

	start()
	t.Cleanup(func() {
		cancel()
		p.OwnerHandle.Close()
		for _, r := range p.Setup.Joined {
			r.Handle.Close()
		}
	})

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
	if !slices.Contains(got, RelayInputID) {
		t.Errorf("shared channel mixer: missing relay input (id=%d); got %v", RelayInputID, got)
	}
}

// mixerOf type-asserts a guild.MixerPauser to *opus.Mixer for test inspection.
func mixerOf(t *testing.T, mp guild.MixerPauser) *opus.Mixer {
	t.Helper()
	mx, ok := mp.(*opus.Mixer)
	if !ok {
		t.Fatalf("ChannelMixers entry is not *opus.Mixer; got %T", mp)
	}
	return mx
}

// typeName returns the package-qualified type name of v (excluding pointer prefix).
func typeName(v any) string {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.String()
}
