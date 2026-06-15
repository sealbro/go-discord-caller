package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// guildCallerPipeline handles RaidModeGuildCaller: every captured channel is a
// source AND a destination, wired mix-minus (each source feeds every dest
// except its own channel). A relay mixer carries the host-side broadcast for
// ally guests; every source feeds relay too.
//
// Replaces mixMinusPipeline. Always-on graph + router decide per-source mode:
//   - router.RouteOff (no callers): empty install
//   - router.RouteCopy (1 source feeding ≤1 non-relay dest): raw Opus to those chOuts
//     plus OpusCallback for ally. Mixer paused. Rare in practice — the §1.1
//     multi-source rule forces mix as soon as ≥2 channels have role-bearing
//     users.
//   - router.RouteMix: per-feed SourceBuffer attached to each destination mixer
//     (including relay). Channel mixers and relay mixer unpause.
type guildCallerPipeline struct{}

func (guildCallerPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	destinations := buildDestinations(p.setup.joined)
	if p.chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}

	// One mixer per destination channel.
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.gm.Opus)
		if err != nil {
			return nil, nil, fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}
	relayMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	// destSlots in router order: per-channel destinations, then relay.
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

	// One router.SourceSlot per capturing source: owner + each deduplicated speaker
	// channel. buildSources already returns this list.
	srcEntries := buildSources(p.ownerBotID, p.ov.ChannelID(), p.ownerHandle, p.setup.joined)
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.id,
			ChannelID: e.channelID,
			Handle:    e.handle,
		}
		// Mix-minus: source feeds every destination except its own channel.
		for _, d := range dests {
			if d.ChannelID == e.channelID {
				continue
			}
			slot.Feeds = append(slot.Feeds, d)
			d.Sources = append(d.Sources, slot)
		}
		guildID := p.guildID
		allySession := p.allySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.gm.Drop(telemetry.DropPathDirect),
			dropMixer:  p.gm.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guildID, pkt)
			},
		})
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.gm
	r := router.New(p.guildID, p.allowFilter.RoleID(), p.voiceProbe, sourceSlots, dests).
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
		GuildID: p.guildID,
		Cancel:  p.cancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.allyCode,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
		AutoRouter:    r,
	}

	start := func() {
		// Channel mixers + relay run unconditionally; the router unpauses them
		// as needed. registerRelayInputs still attaches guest broadcast feeds
		// into host channel mixers when AllyCaller guests are permitted.
		if p.mode.AllowGuestCapture() {
			registerRelayInputs(ctx, p.gm, p.allySession, destinations, channelMixers)
		}
		startChannelMixers(ctx, p.gm, destinations, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
		r.Recompute()
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	return session, start, nil
}
