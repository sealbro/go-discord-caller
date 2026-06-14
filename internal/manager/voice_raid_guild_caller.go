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

// guildCallerPipeline handles RaidModeGuildCaller: every captured channel is a
// source AND a destination, wired mix-minus (each source feeds every dest
// except its own channel). A relay mixer carries the host-side broadcast for
// ally guests; every source feeds relay too.
//
// Replaces mixMinusPipeline. Always-on graph + router decide per-source mode:
//   - routeOff (no callers): empty install
//   - routeCopy (1 source feeding ≤1 non-relay dest): raw Opus to those chOuts
//     plus OpusCallback for ally. Mixer paused. Rare in practice — the §1.1
//     multi-source rule forces mix as soon as ≥2 channels have role-bearing
//     users.
//   - routeMix: per-feed SourceBuffer attached to each destination mixer
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
	dests := make([]*destSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		dests = append(dests, &destSlot{
			channelID: dest.channelID,
			mixer:     channelMixers[dest.channelID],
			chOuts:    dest.outs,
		})
	}
	relaySlot := &destSlot{channelID: relayDestID, mixer: relayMixer}
	dests = append(dests, relaySlot)

	// One sourceSlot per capturing source: owner + each deduplicated speaker
	// channel. buildSources already returns this list.
	srcEntries := buildSources(p.ownerBotID, p.ov.ChannelID(), p.ownerHandle, p.setup.joined)
	sourceSlots := make([]*sourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &sourceSlot{
			id:        e.id,
			channelID: e.channelID,
			handle:    e.handle,
		}
		// Mix-minus: source feeds every destination except its own channel.
		for _, d := range dests {
			if d.channelID == e.channelID {
				continue
			}
			slot.feeds = append(slot.feeds, d)
			d.sources = append(d.sources, slot)
		}
		slot.buildInstall = mixMinusInstallBuilder(p.gm, p.guildID, slot, p.allySession)
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.gm
	router := newSourceRouter(p.guildID, p.allowFilter.RoleID(), p.voiceProbe, sourceSlots, dests).
		withTransitionRecorder(func(from, to routeMode) {
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
			router.Close()
			speakerCleanup()
		},
		AllyCode:      p.allyCode,
		Speakers:      p.setup.speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.allowFilter,
		AutoRouter:    router,
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
		router.Recompute()
	}
	return session, start, nil
}

// mixMinusInstallBuilder returns the per-source buildInstall closure for the
// mix-minus topology. Identical in shape to oneCallerInstallBuilder; the only
// per-mode work is reading the source's current feeds at install time. Mix
// mode delegates to buildPerUserMixSpec for per-user SourceBuffer allocation.
func mixMinusInstallBuilder(gm telemetry.GuildMetrics, guildID snowflake.ID, src *sourceSlot, allySession *ally.Session) func(routeMode, []userBinding) (opus.FanoutInstall, func()) {
	dropDirect := gm.Drop(telemetry.DropPathDirect)
	dropMixer := gm.Drop(telemetry.DropPathMixer)
	return func(mode routeMode, users []userBinding) (opus.FanoutInstall, func()) {
		switch mode {
		case routeOff:
			return opus.FanoutInstall{}, func() {}
		case routeCopy:
			var outs []chan<- []byte
			for _, d := range src.feeds {
				outs = append(outs, d.chOuts...)
			}
			// Every source's feeds include the relay destSlot, so copy mode
			// must mirror the ally broadcast that mix mode would otherwise
			// route through the relay mixer.
			return opus.FanoutInstall{
				OpusTargets: outs,
				OpusCallback: func(pkt []byte) {
					allySession.BroadcastFromGuild(guildID, pkt)
				},
				DropOpus: dropDirect,
			}, func() {}
		case routeMix:
			return buildPerUserMixSpec(src, users, dropMixer)
		}
		return opus.FanoutInstall{}, func() {}
	}
}
