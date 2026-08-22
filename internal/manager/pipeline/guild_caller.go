package pipeline

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

// GuildCallerPipeline handles RaidModeGuildCaller: every captured channel is a
// source AND a destination, wired mix-minus (each source feeds every dest
// except its own channel). A relay mixer carries the host-side broadcast for
// ally guests; every source feeds relay too.
//
// Replaces mixMinusPipeline. Always-on graph + router decide per-source mode:
//   - router.RouteOff (no callers): empty install
//   - router.RouteCopy (1 source feeding ≤1 non-relay dest): raw Opus to those ChOuts
//     plus OpusCallback for ally. Mixer paused. Rare in practice — the §1.1
//     multi-source rule forces mix as soon as ≥2 channels have role-bearing
//     users.
//   - router.RouteMix: per-feed SourceBuffer attached to each destination mixer
//     (including relay). Channel mixers and relay mixer unpause.
type GuildCallerPipeline struct{}

func (GuildCallerPipeline) Build(ctx context.Context, p Params) (*guild.Session, func(), error) {
	destinations := BuildDestinations(p.Setup.Joined)
	if p.ChOwnerOut != nil {
		destinations = append(destinations, &DestChannel{
			ChannelID: p.OV.ChannelID(),
			Outs:      []chan<- []byte{p.ChOwnerOut},
		})
	}

	// One mixer per destination channel.
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.GM.Opus)
		if err != nil {
			return nil, nil, fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.ChannelID] = mx
	}
	relayMixer, err := opus.NewMixer(p.GM.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	// destSlots in router order: per-channel destinations, then relay.
	// Guest audio enters every channel mixer via RegisterRelayInputs below, so
	// each of those destinations carries a RelayFeed predicate — without it the
	// router would pause them whenever this guild is quiet and drop everything
	// the guests relay in (issue #51).
	var relayFeed func() bool
	if p.Mode.AllowGuestCapture() {
		relayFeed = RelayFeedFor(p.AllySession, p.GuildID)
	}
	dests := make([]*router.DestSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		dests = append(dests, &router.DestSlot{
			ChannelID: dest.ChannelID,
			Mixer:     channelMixers[dest.ChannelID],
			ChOuts:    dest.Outs,
			RelayFeed: relayFeed,
		})
	}
	relaySlot := &router.DestSlot{ChannelID: RelayDestID, Mixer: relayMixer}
	dests = append(dests, relaySlot)

	// One router.SourceSlot per capturing source: owner + each deduplicated speaker
	// channel. BuildSources already returns this list.
	srcEntries := BuildSources(p.OwnerBotID, p.OV.ChannelID(), p.OwnerHandle, p.Setup.Joined)
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.ID,
			ChannelID: e.ChannelID,
			Handle:    e.Handle,
		}
		// Mix-minus: source feeds every destination except its own channel.
		for _, d := range dests {
			if d.ChannelID == e.ChannelID {
				continue
			}
			slot.Feeds = append(slot.Feeds, d)
			d.Sources = append(d.Sources, slot)
		}
		guildID := p.GuildID
		allySession := p.AllySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.GM.Drop(telemetry.DropPathDirect),
			dropMixer:  p.GM.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guildID, pkt)
			},
		})
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.GM
	r := router.New(p.GuildID, p.AllowFilter.RoleID(), p.VoiceProbe, sourceSlots, dests).
		WithTransitionRecorder(func(from, to router.RouteMode) {
			gm.RouteTransition(from.String(), to.String())
		})

	mixerPausers := make(map[snowflake.ID]guild.MixerPauser, len(channelMixers)+1)
	for chID, mx := range channelMixers {
		mixerPausers[chID] = mx
	}
	mixerPausers[RelayDestID] = relayMixer

	speakerCleanup := p.Setup.SpeakerCleanup
	session := &guild.Session{
		GuildID: p.GuildID,
		Cancel:  p.CancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.AllyCode,
		Speakers:      p.Setup.Speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.AllowFilter,
		AutoRouter:    r,
	}

	start := func() {
		// Channel mixers + relay run unconditionally; the router unpauses them
		// as needed. RegisterRelayInputs still attaches guest broadcast feeds
		// into host channel mixers when AllyCaller guests are permitted.
		if p.Mode.AllowGuestCapture() {
			RegisterRelayInputs(ctx, p.GM, p.AllySession, destinations, channelMixers)
		}
		// The host always broadcasts, so guests' relay-fed destinations must
		// see it as capturing; the observer re-routes this guild when a guest
		// attaches or leaves.
		WatchRelayMembership(p.AllySession, p.GuildID, r, true)
		StartChannelMixers(ctx, p.GM, destinations, channelMixers, relayFeed)
		StartRelayBroadcast(ctx, p.GM, relayMixer, p.AllySession, p.OwnerCleanup)
		r.Recompute()
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	return session, start, nil
}
