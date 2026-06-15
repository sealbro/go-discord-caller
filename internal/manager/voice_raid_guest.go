package manager

import (
	"context"
	"fmt"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
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
	ownerHandle    *opus.FanoutHandle // non-nil iff the guest owner bot has an inline-capture VoiceReceiver wired up
	guestGm        telemetry.GuildMetrics
	allowFilter    *AllowFilter
	voiceProbe     router.VoiceProbe // production: *cacheVoiceProbe; consumed by the auto-router
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
//
// Auto-route shape: every captured source's only feed is the relay router.DestSlot,
// so copy mode emits zero local OpusTargets but keeps OpusCallback for
// outbound broadcast; mix mode allocates per-user SourceBuffers into the
// relay mixer only.
type guestStarCallerPipeline struct{}

func (guestStarCallerPipeline) build(ctx context.Context, p guestPipelineParams) (*guild.Session, func(), func(), error) {
	allOuts := make([]chan<- []byte, len(p.setup.outs))
	copy(allOuts, p.setup.outs)
	if p.ownerChOut != nil {
		allOuts = append(allOuts, p.ownerChOut)
	}

	relayMixer, err := opus.NewMixer(p.guestGm.Opus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guest star: create relay mixer: %w", err)
	}
	relaySlot := &router.DestSlot{ChannelID: relayDestID, Mixer: relayMixer}
	dests := []*router.DestSlot{relaySlot}

	srcEntries := buildGuestSources(p.setup.joined)
	if p.ownerHandle != nil {
		srcEntries = append(srcEntries, sourceEntry{p.ownerBotID, p.ownerChannelID, p.ownerHandle})
	}
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.id,
			ChannelID: e.channelID,
			Handle:    e.handle,
			Feeds:     []*router.DestSlot{relaySlot},
		}
		guestGuildID := p.guestGuildID
		allySession := p.allySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.guestGm.Drop(telemetry.DropPathDirect),
			dropMixer:  p.guestGm.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guestGuildID, pkt)
			},
		})
		relaySlot.Sources = append(relaySlot.Sources, slot)
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.guestGm
	r := router.New(p.guestGuildID, p.allowFilter.RoleID(), p.voiceProbe, sourceSlots, dests).
		WithTransitionRecorder(func(from, to router.RouteMode) {
			gm.RouteTransition(from.String(), to.String())
		})

	mixerPausers := map[snowflake.ID]guild.MixerPauser{relayDestID: relayMixer}

	speakerCleanup := p.setup.speakerCleanup
	session := &guild.Session{
		GuildID: p.guestGuildID,
		Cancel:  p.cancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.code,
		IsGuest:       true,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
		AutoRouter:    r,
	}

	start := func() {
		// Local destination chOuts receive audio from the host's broadcast
		// via AddGuild → ally session → speaker chOut. The router only
		// handles the outbound (capture → relay) side.
		p.allySession.AddGuild(p.guestGuildID, allOuts)
		startGuestRelayBroadcast(ctx, relayMixer, p.allySession, p.guestGuildID)
		r.Recompute()
	}
	cleanup := func() {
		for _, ch := range allOuts {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}

// guestCallerPipeline handles RaidModeAllyCaller: full mix-minus, one channel
// mixer per destination, relay mixer for outbound. Mirrors guildCallerPipeline
// for the guest side — same router pattern, same closure shape; the only
// difference is that BroadcastFromGuild fires with guestGuildID so the packet
// reaches the host and every other guest.
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

	dests := make([]*router.DestSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		dests = append(dests, &router.DestSlot{
			ChannelID: dest.channelID,
			Mixer:     channelMixers[dest.channelID],
			ChOuts:    dest.outs,
		})
	}
	relaySlot := &router.DestSlot{ChannelID: relayDestID, Mixer: relayMixer}
	dests = append(dests, relaySlot)

	srcEntries := buildGuestSources(p.setup.joined)
	if p.ownerHandle != nil {
		srcEntries = append(srcEntries, sourceEntry{p.ownerBotID, p.ownerChannelID, p.ownerHandle})
	}
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.id,
			ChannelID: e.channelID,
			Handle:    e.handle,
		}
		for _, d := range dests {
			if d.ChannelID == e.channelID {
				continue
			}
			slot.Feeds = append(slot.Feeds, d)
			d.Sources = append(d.Sources, slot)
		}
		guestGuildID := p.guestGuildID
		allySession := p.allySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.guestGm.Drop(telemetry.DropPathDirect),
			dropMixer:  p.guestGm.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guestGuildID, pkt)
			},
		})
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.guestGm
	r := router.New(p.guestGuildID, p.allowFilter.RoleID(), p.voiceProbe, sourceSlots, dests).
		WithTransitionRecorder(func(from, to router.RouteMode) {
			gm.RouteTransition(from.String(), to.String())
		})

	mixerPausers := make(map[snowflake.ID]guild.MixerPauser, len(channelMixers)+1)
	for chID, mx := range channelMixers {
		mixerPausers[chID] = mx
	}
	mixerPausers[relayDestID] = relayMixer

	speakerCleanup := p.setup.speakerCleanup
	session := &guild.Session{
		GuildID: p.guestGuildID,
		Cancel:  p.cancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.code,
		IsGuest:       true,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
		AutoRouter:    r,
	}

	// relayInputs is populated inside start() and closed by cleanup(); safe
	// because the teardown goroutine in JoinSession only runs after start().
	var relayInputs []chan<- []byte
	start := func() {
		relayInputs = registerRelayInputs(ctx, p.guestGm, p.allySession, destinations, channelMixers)
		startChannelMixers(ctx, p.guestGm, destinations, channelMixers)
		startGuestRelayBroadcast(ctx, relayMixer, p.allySession, p.guestGuildID)
		r.Recompute()
	}
	cleanup := func() {
		for _, ch := range relayInputs {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}
