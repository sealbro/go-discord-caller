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

// starCallerPipeline handles RaidModeOneManyGuildCaller. Star topology with
// the owner channel as the hub:
//
//   - Owner source → raw Opus directly to every speaker chOut (no per-speaker
//     mixer needed) AND, when running, a decoded frame to the relay mixer for
//     ally broadcast.
//   - Speaker source → decoded frame into the hub mixer (whose sink writes the
//     mix into chOwnerOut for owner playback). Speakers are NOT broadcast to
//     ally guests — only owner/caller audio crosses the relay.
//
// Replaces starPipeline. Like the other host pipelines the channel mixers
// (hub + relay) are created up front and started paused; the router decides
// per-source mode based on caller counts.
//
// Mode semantics for star differ from OneCaller / GuildCaller in two ways:
//   - Owner ALWAYS emits raw Opus to speaker chOuts (in both copy and mix
//     modes) since there are no per-speaker mixers to drive.
//   - Speakers do not include an OpusCallback for ally in copy mode — ally
//     receives only what crosses the relay mixer, which only the owner feeds.
type starCallerPipeline struct{}

func (starCallerPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	srcEntries := buildSources(p.ownerBotID, p.ov.ChannelID(), p.ownerHandle, p.setup.joined)
	destinations := buildDestinations(p.setup.joined)
	// Partition destinations: owner hub is the only mixer-driven destination.
	// All other dests' chOuts become raw OpusTargets for the owner source.
	var ownerDests []*destChannel
	var speakerOuts []chan<- []byte
	for _, dest := range destinations {
		if dest.channelID == p.ov.ChannelID() {
			ownerDests = append(ownerDests, dest)
		} else {
			speakerOuts = append(speakerOuts, dest.outs...)
		}
	}
	if p.chOwnerOut != nil {
		ownerDests = append(ownerDests, &destChannel{
			channelID: p.ov.ChannelID(),
			outs:      []chan<- []byte{p.chOwnerOut},
		})
	}

	hubMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create hub mixer: %w", err)
	}
	relayMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	// Collect hub chOuts. After partitioning above, every ownerDests entry is
	// at the owner channel — concatenate their outs as the hub destSlot's
	// chOuts (the mixer's sink writes the mix into them).
	var hubChOuts []chan<- []byte
	for _, d := range ownerDests {
		hubChOuts = append(hubChOuts, d.outs...)
	}
	hubSlot := &destSlot{
		channelID: p.ov.ChannelID(),
		mixer:     hubMixer,
		chOuts:    hubChOuts,
	}
	relaySlot := &destSlot{channelID: relayDestID, mixer: relayMixer}
	dests := []*destSlot{hubSlot, relaySlot}

	// Per-source slots. Owner feeds relay only (speakers receive raw via
	// OpusTargets, handled inside the install closure). Speakers feed hub
	// only (no ally relay).
	sourceSlots := make([]*sourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &sourceSlot{
			id:        e.id,
			channelID: e.channelID,
			handle:    e.handle,
		}
		if e.channelID == p.ov.ChannelID() {
			slot.feeds = []*destSlot{relaySlot}
			relaySlot.sources = append(relaySlot.sources, slot)
			slot.buildInstall = ownerStarInstallBuilder(p.gm, p.guildID, slot, p.allySession, speakerOuts)
		} else {
			slot.feeds = []*destSlot{hubSlot}
			hubSlot.sources = append(hubSlot.sources, slot)
			slot.buildInstall = speakerStarInstallBuilder(p.gm, slot)
		}
		sourceSlots = append(sourceSlots, slot)
	}

	router := newSourceRouter(p.guildID, p.allowFilter.RoleID(), p.voiceProbe, sourceSlots, dests)

	mixerPausers := map[snowflake.ID]guild.MixerPauser{
		p.ov.ChannelID(): hubMixer,
		relayDestID:      relayMixer,
	}

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
		if p.mode.AllowGuestCapture() {
			// Guest broadcasts enter at the hub mixer only — speakers don't
			// receive guest audio in star mode.
			channelMixers := map[snowflake.ID]*opus.Mixer{p.ov.ChannelID(): hubMixer}
			registerRelayInputs(ctx, p.gm, p.allySession, ownerDests, channelMixers)
		}
		channelMixers := map[snowflake.ID]*opus.Mixer{p.ov.ChannelID(): hubMixer}
		startChannelMixers(ctx, p.gm, ownerDests, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
		router.Recompute()
	}
	return session, start, nil
}

// ownerStarInstallBuilder returns the owner source's buildInstall closure.
// Owner emits raw Opus to speaker chOuts in BOTH copy and mix modes (no
// per-speaker mixers exist to feed). Ally broadcast goes via OpusCallback in
// copy mode and via the relay mixer SourceBuffer(s) in mix mode (one per
// role-bearing user in the owner channel — §4.3 per-user keying).
func ownerStarInstallBuilder(gm telemetry.GuildMetrics, guildID snowflake.ID, src *sourceSlot, allySession *ally.Session, speakerOuts []chan<- []byte) func(routeMode, []userBinding) (opus.FanoutInstall, func()) {
	dropDirect := gm.Drop(telemetry.DropPathDirect)
	dropMixer := gm.Drop(telemetry.DropPathMixer)
	return func(mode routeMode, users []userBinding) (opus.FanoutInstall, func()) {
		switch mode {
		case routeOff:
			return opus.FanoutInstall{}, func() {}
		case routeCopy:
			return opus.FanoutInstall{
				OpusTargets: speakerOuts,
				OpusCallback: func(pkt []byte) {
					allySession.BroadcastFromGuild(guildID, pkt)
				},
				DropOpus: dropDirect,
			}, func() {}
		case routeMix:
			spec, teardown := buildPerUserMixSpec(src, users, dropMixer)
			// Raw OpusTargets remain in mix mode too — there is no
			// per-speaker-channel mixer for these chOuts.
			spec.OpusTargets = speakerOuts
			spec.DropOpus = dropDirect
			return spec, teardown
		}
		return opus.FanoutInstall{}, func() {}
	}
}

// speakerStarInstallBuilder returns a speaker source's buildInstall closure
// for the star topology. Speakers feed the hub mixer only and do not
// participate in the ally broadcast — OpusCallback is intentionally absent.
func speakerStarInstallBuilder(gm telemetry.GuildMetrics, src *sourceSlot) func(routeMode, []userBinding) (opus.FanoutInstall, func()) {
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
			return opus.FanoutInstall{OpusTargets: outs}, func() {}
		case routeMix:
			return buildPerUserMixSpec(src, users, dropMixer)
		}
		return opus.FanoutInstall{}, func() {}
	}
}
