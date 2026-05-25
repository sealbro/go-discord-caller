package manager

import (
	"context"
	"fmt"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// guestPipelineParams holds all inputs the three guest pipeline topologies need.
// Built once by JoinSession after the common owner/speaker setup.
type guestPipelineParams struct {
	guestGuildID   snowflake.ID
	ownerBotID     snowflake.ID
	ownerChannelID snowflake.ID // zero when owner bot could not join
	cancelFunc     context.CancelFunc
	code           ally.Code
	guestMode      guild.RaidMode
	allySession    *ally.Session
	setup          *raidSetup
	ownerChOut     chan []byte
	ownerChIn      chan []byte
	ownerHandle    *opus.FanoutHandle
	guestGm        telemetry.GuildMetrics
	allowFilter    *AllowFilter
}

// guestPipeline builds the audio wiring for one guest topology and returns the
// session, a start func to call after commitSession, a cleanup func to call on
// teardown, and an error. On failure the caller is responsible for running
// speaker/owner cleanup and removing the guest from the session registry.
type guestPipeline interface {
	build(ctx context.Context, p guestPipelineParams) (session *guild.Session, start func(), cleanup func(), err error)
}

// guestPipelineFor returns the correct guest pipeline implementation for mode.
func guestPipelineFor(mode guild.RaidMode) guestPipeline {
	switch {
	case !mode.WithCapture():
		return guestListenerPipeline{}
	case mode.IsStarTopology():
		return guestStarCallerPipeline{}
	default:
		return guestCallerPipeline{}
	}
}

// guestListenerPipeline handles RaidModeAllyListener: raw Opus relay, no local mixing.
type guestListenerPipeline struct{}

func (guestListenerPipeline) build(_ context.Context, p guestPipelineParams) (*guild.Session, func(), func(), error) {
	outs := make([]chan<- []byte, len(p.setup.outs))
	copy(outs, p.setup.outs)
	if p.ownerChOut != nil {
		outs = append(outs, p.ownerChOut)
	}
	session := &guild.Session{
		GuildID:     p.guestGuildID,
		Cancel:      p.cancelFunc,
		Cleanup:     p.setup.speakerCleanup,
		AllyCode:    p.code,
		IsGuest:     true,
		Speakers:    p.setup.speakers,
		AllowFilter: p.allowFilter,
	}
	start := func() {
		p.allySession.AddGuild(p.guestGuildID, outs)
	}
	cleanup := func() {
		for _, ch := range outs {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}

// guestStarCallerPipeline handles RaidModeOneManyAllyCaller: sources → relay only,
// host relay delivered directly to speaker chOuts (no local channel mixers).
type guestStarCallerPipeline struct{}

func (guestStarCallerPipeline) build(ctx context.Context, p guestPipelineParams) (*guild.Session, func(), func(), error) {
	destinations := buildDestinations(p.setup.joined)
	if p.ownerChOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ownerChannelID,
			outs:      []chan<- []byte{p.ownerChOut},
		})
	}
	relayMixer, err := opus.NewMixer(p.guestGm.Opus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guest star: create relay mixer: %w", err)
	}

	allOuts := make([]chan<- []byte, len(p.setup.outs))
	copy(allOuts, p.setup.outs)
	if p.ownerChOut != nil {
		allOuts = append(allOuts, p.ownerChOut)
	}

	session := &guild.Session{
		GuildID:     p.guestGuildID,
		Cancel:      p.cancelFunc,
		Cleanup:     p.setup.speakerCleanup,
		AllyCode:    p.code,
		IsGuest:     true,
		Speakers:    p.setup.speakers,
		AllowFilter: p.allowFilter,
	}

	sources := buildGuestSources(ctx, p.setup.joined)
	if p.ownerChIn != nil {
		sources = append(sources, sourceEntry{p.ownerBotID, p.ownerChannelID, p.ownerHandle})
	}

	start := func() {
		// ownerChannelID=0: all sources go to relay mixer only.
		wireFanoutOneMany(ctx, p.guestGm, sources, destinations, nil, relayMixer, 0)
		p.allySession.AddGuild(p.guestGuildID, allOuts)
		startGuestRelayBroadcast(ctx, relayMixer, p.allySession, p.guestGuildID)
	}
	cleanup := func() {
		for _, ch := range allOuts {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}

// guestCallerPipeline handles RaidModeAllyCaller: full mix-minus, one channel mixer per
// destination, relay mixer for outbound. Mirrors the host mixMinusPipeline for guest guilds.
type guestCallerPipeline struct{}

func (guestCallerPipeline) build(ctx context.Context, p guestPipelineParams) (*guild.Session, func(), func(), error) {
	destinations := buildDestinations(p.setup.joined)
	if p.ownerChOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ownerChannelID,
			outs:      []chan<- []byte{p.ownerChOut},
		})
	}

	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.guestGm.Opus)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("guest caller: create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}
	relayMixer, err := opus.NewMixer(p.guestGm.Opus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guest caller: create relay mixer: %w", err)
	}

	mixerPausers := make(map[snowflake.ID]guild.MixerPauser, len(channelMixers))
	for chID, mx := range channelMixers {
		mixerPausers[chID] = mx
	}

	session := &guild.Session{
		GuildID:       p.guestGuildID,
		Cancel:        p.cancelFunc,
		Cleanup:       p.setup.speakerCleanup,
		AllyCode:      p.code,
		IsGuest:       true,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
	}

	sources := buildGuestSources(ctx, p.setup.joined)
	if p.ownerChIn != nil {
		sources = append(sources, sourceEntry{p.ownerBotID, p.ownerChannelID, p.ownerHandle})
	}

	// relayInputs is populated inside start() and closed by cleanup(); it is safe
	// because the teardown goroutine in JoinSession only runs after start() returns.
	var relayInputs []chan<- []byte
	start := func() {
		wireFanout(ctx, p.guestGm, sources, destinations, channelMixers, relayMixer)
		relayInputs = registerRelayInputs(ctx, p.guestGm, p.allySession, destinations, channelMixers)
		startChannelMixers(ctx, p.guestGm, destinations, channelMixers)
		startGuestRelayBroadcast(ctx, relayMixer, p.allySession, p.guestGuildID)
	}
	cleanup := func() {
		for _, ch := range relayInputs {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}
