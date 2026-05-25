package manager

import (
	"context"
	"fmt"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// pipelineParams holds all inputs that the three host pipeline topologies need.
// Built once by StartVoiceRaid after the common owner/speaker setup; passed to hostPipeline.build.
type pipelineParams struct {
	guildID      snowflake.ID
	ownerBotID   snowflake.ID
	cancelFunc   context.CancelFunc
	mode         guild.RaidMode
	allyCode     ally.Code
	allySession  *ally.Session
	setup        *raidSetup
	chIn         chan []byte        // owner capture channel; used by directPipeline (RaidModeOneCaller bypass only)
	ownerHandle  *opus.FanoutHandle // owner FanoutHandle; non-nil when owner capture is enabled
	chOwnerOut   chan []byte        // owner playback channel; nil for direct passthrough (RaidModeOneCaller)
	ownerCleanup func()             // closes owner provider/receiver; called on teardown or build error
	ov           pool.GuildVoice
	gm           telemetry.GuildMetrics
	allowFilter  *AllowFilter
}

// hostPipeline builds the audio wiring for one topology and returns the
// session to commit plus a start func to call after commitSession + syncMixerPauseState.
// On failure it returns a non-nil error; the caller is responsible for cleaning up
// speakers, the owner voice connection, and the ally session.
type hostPipeline interface {
	build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error)
}

// pipelineFor returns the correct pipeline implementation for mode.
func pipelineFor(mode guild.RaidMode) hostPipeline {
	switch {
	case mode.IsDirectPassthrough():
		return directPipeline{}
	case mode.IsStarTopology():
		return starPipeline{}
	default:
		return mixMinusPipeline{}
	}
}

// chain composes multiple start functions into one ordered sequence.
func chain(fns ...func()) func() {
	return func() {
		for _, fn := range fns {
			fn()
		}
	}
}

// directPipeline handles RaidModeOneCaller: single source, raw Opus passthrough — no mixing.
type directPipeline struct{}

func (directPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	session := &guild.Session{
		GuildID:     p.guildID,
		Cancel:      p.cancelFunc,
		Cleanup:     p.setup.speakerCleanup,
		AllyCode:    p.allyCode,
		Speakers:    p.setup.speakers,
		AllowFilter: p.allowFilter,
		// ChannelMixers intentionally nil: UpdateMixerPause guards for nil.
	}
	start := chain(
		func() { startFanoutDirect(ctx, p.gm, p.chIn, p.setup.outs, p.allySession) },
		func() { startDirectSessionCleanup(ctx, p.gm, p.ownerCleanup) },
	)
	return session, start, nil
}

// starPipeline handles RaidModeOneManyGuildCaller: hub mixer at the owner channel only.
// Speaker channels receive raw Opus frames directly — no per-channel mixer needed.
type starPipeline struct{}

func (starPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	sources := buildSources(ctx, p.ownerBotID, p.ov.ChannelID(), p.ownerHandle, p.setup.joined)
	destinations := buildDestinations(p.setup.joined)
	if p.chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}
	relayMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}
	hubMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create hub mixer: %w", err)
	}
	channelMixers := map[snowflake.ID]*opus.Mixer{p.ov.ChannelID(): hubMixer}
	mixerPausers := map[snowflake.ID]guild.MixerPauser{p.ov.ChannelID(): hubMixer}
	session := &guild.Session{
		GuildID:       p.guildID,
		Cancel:        p.cancelFunc,
		Cleanup:       p.setup.speakerCleanup,
		AllyCode:      p.allyCode,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
	}
	// Partition destinations: owner hub gets mixed output; all other channels
	// get raw Opus directly from the fanout goroutine (no mixing needed).
	var ownerDests []*destChannel
	var directSpeakerOuts []chan<- []byte
	for _, dest := range destinations {
		if dest.channelID == p.ov.ChannelID() {
			ownerDests = append(ownerDests, dest)
		} else {
			directSpeakerOuts = append(directSpeakerOuts, dest.outs...)
		}
	}
	start := func() {
		wireFanoutOneManyDirect(ctx, p.gm, sources, p.ov.ChannelID(), directSpeakerOuts, channelMixers, relayMixer)
		// Guest relay enters only at the hub mixer.
		if p.mode.AllowGuestCapture() {
			registerRelayInputs(ctx, p.gm, p.allySession, ownerDests, channelMixers)
		}
		startChannelMixers(ctx, p.gm, ownerDests, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
	}
	return session, start, nil
}

// mixMinusPipeline handles RaidModeGuildCaller: full mix-minus with one mixer per destination channel.
type mixMinusPipeline struct{}

func (mixMinusPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	sources := buildSources(ctx, p.ownerBotID, p.ov.ChannelID(), p.ownerHandle, p.setup.joined)
	destinations := buildDestinations(p.setup.joined)
	if p.chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}
	relayMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.gm.Opus)
		if err != nil {
			return nil, nil, fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}
	mixerPausers := make(map[snowflake.ID]guild.MixerPauser, len(channelMixers))
	for chID, mx := range channelMixers {
		mixerPausers[chID] = mx
	}
	session := &guild.Session{
		GuildID:       p.guildID,
		Cancel:        p.cancelFunc,
		Cleanup:       p.setup.speakerCleanup,
		AllyCode:      p.allyCode,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
	}
	start := func() {
		wireFanout(ctx, p.gm, sources, destinations, channelMixers, relayMixer)
		// When the host allows guest capture, register host channel mixers as relay
		// receivers so BroadcastFromGuild packets from AllyCaller guests reach host speakers.
		if p.mode.AllowGuestCapture() {
			registerRelayInputs(ctx, p.gm, p.allySession, destinations, channelMixers)
		}
		startChannelMixers(ctx, p.gm, destinations, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
	}
	return session, start, nil
}
