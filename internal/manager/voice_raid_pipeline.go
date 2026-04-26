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
	chIn         chan []byte // owner capture channel (output of VoiceReceiver)
	chOwnerOut   chan []byte // owner playback channel; nil for direct passthrough (RaidModeOneCaller)
	ownerCleanup func()      // closes owner provider/receiver; called on teardown or build error
	ov           pool.GuildVoice
	metrics      *telemetry.Metrics
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
	start := func() {
		startFanoutDirect(ctx, p.chIn, p.setup.outs, p.allySession, p.guildID, &p.metrics.Session)
		startDirectSessionCleanup(ctx, p.ownerCleanup, p.guildID, &p.metrics.Session)
	}
	return session, start, nil
}

// starPipeline handles RaidModeOneManyGuildCaller: hub mixer at the owner channel only.
// Speaker channels receive raw Opus frames directly — no per-channel mixer needed.
type starPipeline struct{}

func (starPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	sources := buildSources(ctx, p.ownerBotID, p.ov.ChannelID(), p.chIn, p.setup.joined)
	destinations := buildDestinations(p.setup.joined)
	if p.chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}
	relayMixer, err := opus.NewMixer(p.metrics.Opus.For(p.guildID.String()))
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}
	hubMixer, err := opus.NewMixer(p.metrics.Opus.For(p.guildID.String()))
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
		wireFanoutOneManyDirect(ctx, p.guildID, sources, p.ov.ChannelID(), directSpeakerOuts, channelMixers, relayMixer, &p.metrics.Session)
		// Guest relay enters only at the hub mixer.
		if p.mode.AllowGuestCapture() {
			registerRelayInputs(ctx, p.guildID, p.allySession, ownerDests, channelMixers, &p.metrics.Session)
		}
		startChannelMixers(ctx, ownerDests, channelMixers, p.guildID, &p.metrics.Session)
		startRelayBroadcast(ctx, relayMixer, p.allySession, p.ownerCleanup, p.guildID, &p.metrics.Session)
	}
	return session, start, nil
}

// mixMinusPipeline handles RaidModeGuildCaller: full mix-minus with one mixer per destination channel.
type mixMinusPipeline struct{}

func (mixMinusPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	sources := buildSources(ctx, p.ownerBotID, p.ov.ChannelID(), p.chIn, p.setup.joined)
	destinations := buildDestinations(p.setup.joined)
	if p.chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}
	relayMixer, err := opus.NewMixer(p.metrics.Opus.For(p.guildID.String()))
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.metrics.Opus.For(p.guildID.String()))
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
		wireFanout(ctx, p.guildID, sources, destinations, channelMixers, relayMixer, &p.metrics.Session)
		// When the host allows guest capture, register host channel mixers as relay
		// receivers so BroadcastFromGuild packets from AllyCaller guests reach host speakers.
		if p.mode.AllowGuestCapture() {
			registerRelayInputs(ctx, p.guildID, p.allySession, destinations, channelMixers, &p.metrics.Session)
		}
		startChannelMixers(ctx, destinations, channelMixers, p.guildID, &p.metrics.Session)
		startRelayBroadcast(ctx, relayMixer, p.allySession, p.ownerCleanup, p.guildID, &p.metrics.Session)
	}
	return session, start, nil
}
