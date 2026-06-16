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

// StarCallerPipeline handles RaidModeOneManyGuildCaller. Star topology with
// the owner channel as the hub:
//
//   - Owner source → raw Opus directly to every speaker chOut (no per-speaker
//     mixer needed) AND, when running, a decoded frame to the relay mixer for
//     ally broadcast.
//   - Speaker source → decoded frame into the hub mixer (whose sink writes the
//     mix into ChOwnerOut for owner playback). Speakers are NOT broadcast to
//     ally guests — only owner/caller audio crosses the relay.
//
// Replaces starPipeline. Like the other host pipelines the channel mixers
// (hub + relay) are created up front and started paused; the router decides
// per-source mode based on caller counts.
//
// Mode semantics for star differ from OneCaller / GuildCaller in two ways:
//   - Owner ALWAYS emits raw Opus to speaker ChOuts (in both copy and mix
//     modes) since there are no per-speaker mixers to drive.
//   - Speakers do not include an OpusCallback for ally in copy mode — ally
//     receives only what crosses the relay mixer, which only the owner feeds.
type StarCallerPipeline struct{}

func (StarCallerPipeline) Build(ctx context.Context, p Params) (*guild.Session, func(), error) {
	srcEntries := BuildSources(p.OwnerBotID, p.OV.ChannelID(), p.OwnerHandle, p.Setup.Joined)
	destinations := BuildDestinations(p.Setup.Joined)
	// Partition destinations: owner hub is the only mixer-driven destination.
	// All other dests' ChOuts become raw OpusTargets for the owner source.
	var ownerDests []*DestChannel
	var speakerOuts []chan<- []byte
	for _, dest := range destinations {
		if dest.ChannelID == p.OV.ChannelID() {
			ownerDests = append(ownerDests, dest)
		} else {
			speakerOuts = append(speakerOuts, dest.Outs...)
		}
	}
	if p.ChOwnerOut != nil {
		ownerDests = append(ownerDests, &DestChannel{
			ChannelID: p.OV.ChannelID(),
			Outs:      []chan<- []byte{p.ChOwnerOut},
		})
	}

	hubMixer, err := opus.NewMixer(p.GM.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create hub mixer: %w", err)
	}
	relayMixer, err := opus.NewMixer(p.GM.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	// Collect hub ChOuts. After partitioning above, every ownerDests entry is
	// at the owner channel — concatenate their outs as the hub router.DestSlot's
	// ChOuts (the mixer's sink writes the mix into them).
	var hubChOuts []chan<- []byte
	for _, d := range ownerDests {
		hubChOuts = append(hubChOuts, d.Outs...)
	}
	hubSlot := &router.DestSlot{
		ChannelID: p.OV.ChannelID(),
		Mixer:     hubMixer,
		ChOuts:    hubChOuts,
	}
	relaySlot := &router.DestSlot{ChannelID: RelayDestID, Mixer: relayMixer}
	dests := []*router.DestSlot{hubSlot, relaySlot}

	// Per-source slots. Owner feeds relay only (speakers receive raw via
	// OpusTargets, handled inside the install closure). Speakers feed hub
	// only (no ally relay).
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.ID,
			ChannelID: e.ChannelID,
			Handle:    e.Handle,
		}
		if e.ChannelID == p.OV.ChannelID() {
			slot.Feeds = []*router.DestSlot{relaySlot}
			relaySlot.Sources = append(relaySlot.Sources, slot)
			guildID := p.GuildID
			allySession := p.AllySession
			slot.BuildInstall = routerInstallBuilder(installBuildOpts{
				src:        slot,
				dropDirect: p.GM.Drop(telemetry.DropPathDirect),
				dropMixer:  p.GM.Drop(telemetry.DropPathMixer),
				// Raw Opus stays on the speaker ChOuts in both copy and mix
				// modes — star has no per-speaker mixer to feed.
				rawTargets: speakerOuts,
				allyBroadcast: func(pkt []byte) {
					allySession.BroadcastFromGuild(guildID, pkt)
				},
			})
		} else {
			slot.Feeds = []*router.DestSlot{hubSlot}
			hubSlot.Sources = append(hubSlot.Sources, slot)
			slot.BuildInstall = routerInstallBuilder(installBuildOpts{
				src:       slot,
				dropMixer: p.GM.Drop(telemetry.DropPathMixer),
				// allyBroadcast nil: star speakers do NOT participate in the
				// ally broadcast — only the owner's voice crosses the relay.
			})
		}
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.GM
	r := router.New(p.GuildID, p.AllowFilter.RoleID(), p.VoiceProbe, sourceSlots, dests).
		WithTransitionRecorder(func(from, to router.RouteMode) {
			gm.RouteTransition(from.String(), to.String())
		})

	mixerPausers := map[snowflake.ID]guild.MixerPauser{
		p.OV.ChannelID(): hubMixer,
		RelayDestID:      relayMixer,
	}

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
		if p.Mode.AllowGuestCapture() {
			// Guest broadcasts enter at the hub mixer only — speakers don't
			// receive guest audio in star mode.
			channelMixers := map[snowflake.ID]*opus.Mixer{p.OV.ChannelID(): hubMixer}
			RegisterRelayInputs(ctx, p.GM, p.AllySession, ownerDests, channelMixers)
		}
		channelMixers := map[snowflake.ID]*opus.Mixer{p.OV.ChannelID(): hubMixer}
		StartChannelMixers(ctx, p.GM, ownerDests, channelMixers)
		StartRelayBroadcast(ctx, p.GM, relayMixer, p.AllySession, p.OwnerCleanup)
		r.Recompute()
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	return session, start, nil
}
