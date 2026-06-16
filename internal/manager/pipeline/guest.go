package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/telemetry"

	"github.com/sealbro/go-discord-caller/internal/opus"
)

// GuestListenerPipeline handles RaidModeAllyListener: raw Opus relay, no local mixing.
type GuestListenerPipeline struct{}

func (GuestListenerPipeline) Build(_ context.Context, p GuestParams) (*guild.Session, func(), func(), error) {
	outs := make([]chan<- []byte, len(p.Setup.Outs))
	copy(outs, p.Setup.Outs)
	if p.OwnerChOut != nil {
		outs = append(outs, p.OwnerChOut)
	}
	session := &guild.Session{
		GuildID:     p.GuestGuildID,
		Cancel:      p.CancelFunc,
		Cleanup:     p.Setup.SpeakerCleanup,
		AllyCode:    p.Code,
		IsGuest:     true,
		Speakers:    p.Setup.Speakers,
		AllowFilter: p.AllowFilter,
	}
	start := func() {
		p.AllySession.AddGuild(p.GuestGuildID, outs)
	}
	cleanup := func() {
		for _, ch := range outs {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}

// GuestStarCallerPipeline handles RaidModeOneManyAllyCaller: sources → relay only,
// host relay delivered directly to speaker ChOuts (no local channel mixers).
//
// Auto-route shape: every captured source's only feed is the relay router.DestSlot,
// so copy mode emits zero local OpusTargets but keeps OpusCallback for
// outbound broadcast; mix mode allocates per-user SourceBuffers into the
// relay mixer only.
type GuestStarCallerPipeline struct{}

func (GuestStarCallerPipeline) Build(ctx context.Context, p GuestParams) (*guild.Session, func(), func(), error) {
	allOuts := make([]chan<- []byte, len(p.Setup.Outs))
	copy(allOuts, p.Setup.Outs)
	if p.OwnerChOut != nil {
		allOuts = append(allOuts, p.OwnerChOut)
	}

	relayMixer, err := opus.NewMixer(p.GuestGM.Opus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guest star: create relay mixer: %w", err)
	}
	relaySlot := &router.DestSlot{ChannelID: RelayDestID, Mixer: relayMixer}
	dests := []*router.DestSlot{relaySlot}

	srcEntries := BuildGuestSources(p.Setup.Joined)
	if p.OwnerHandle != nil {
		srcEntries = append(srcEntries, SourceEntry{p.OwnerBotID, p.OwnerChannelID, p.OwnerHandle})
	}
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.ID,
			ChannelID: e.ChannelID,
			Handle:    e.Handle,
			Feeds:     []*router.DestSlot{relaySlot},
		}
		guestGuildID := p.GuestGuildID
		allySession := p.AllySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.GuestGM.Drop(telemetry.DropPathDirect),
			dropMixer:  p.GuestGM.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guestGuildID, pkt)
			},
		})
		relaySlot.Sources = append(relaySlot.Sources, slot)
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.GuestGM
	r := router.New(p.GuestGuildID, p.AllowFilter.RoleID(), p.VoiceProbe, sourceSlots, dests).
		WithTransitionRecorder(func(from, to router.RouteMode) {
			gm.RouteTransition(from.String(), to.String())
		})

	mixerPausers := map[snowflake.ID]guild.MixerPauser{RelayDestID: relayMixer}

	speakerCleanup := p.Setup.SpeakerCleanup
	session := &guild.Session{
		GuildID: p.GuestGuildID,
		Cancel:  p.CancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.Code,
		IsGuest:       true,
		Speakers:      p.Setup.Speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.AllowFilter,
		AutoRouter:    r,
	}

	start := func() {
		// Local destination ChOuts receive audio from the host's broadcast
		// via AddGuild → ally session → speaker chOut. The router only
		// handles the outbound (capture → relay) side.
		p.AllySession.AddGuild(p.GuestGuildID, allOuts)
		StartGuestRelayBroadcast(ctx, relayMixer, p.AllySession, p.GuestGuildID)
		r.Recompute()
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	cleanup := func() {
		for _, ch := range allOuts {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}

// GuestCallerPipeline handles RaidModeAllyCaller: full mix-minus, one channel
// mixer per destination, relay mixer for outbound. Mirrors GuildCallerPipeline
// for the guest side — same router pattern, same closure shape; the only
// difference is that BroadcastFromGuild fires with guestGuildID so the packet
// reaches the host and every other guest.
type GuestCallerPipeline struct{}

func (GuestCallerPipeline) Build(ctx context.Context, p GuestParams) (*guild.Session, func(), func(), error) {
	destinations := BuildDestinations(p.Setup.Joined)
	if p.OwnerChOut != nil {
		destinations = append(destinations, &DestChannel{
			ChannelID: p.OwnerChannelID,
			Outs:      []chan<- []byte{p.OwnerChOut},
		})
	}

	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.GuestGM.Opus)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("guest caller: create channel mixer: %w", err)
		}
		channelMixers[dest.ChannelID] = mx
	}
	relayMixer, err := opus.NewMixer(p.GuestGM.Opus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guest caller: create relay mixer: %w", err)
	}

	dests := make([]*router.DestSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		dests = append(dests, &router.DestSlot{
			ChannelID: dest.ChannelID,
			Mixer:     channelMixers[dest.ChannelID],
			ChOuts:    dest.Outs,
		})
	}
	relaySlot := &router.DestSlot{ChannelID: RelayDestID, Mixer: relayMixer}
	dests = append(dests, relaySlot)

	srcEntries := BuildGuestSources(p.Setup.Joined)
	if p.OwnerHandle != nil {
		srcEntries = append(srcEntries, SourceEntry{p.OwnerBotID, p.OwnerChannelID, p.OwnerHandle})
	}
	sourceSlots := make([]*router.SourceSlot, 0, len(srcEntries))
	for _, e := range srcEntries {
		slot := &router.SourceSlot{
			ID:        e.ID,
			ChannelID: e.ChannelID,
			Handle:    e.Handle,
		}
		for _, d := range dests {
			if d.ChannelID == e.ChannelID {
				continue
			}
			slot.Feeds = append(slot.Feeds, d)
			d.Sources = append(d.Sources, slot)
		}
		guestGuildID := p.GuestGuildID
		allySession := p.AllySession
		slot.BuildInstall = routerInstallBuilder(installBuildOpts{
			src:        slot,
			dropDirect: p.GuestGM.Drop(telemetry.DropPathDirect),
			dropMixer:  p.GuestGM.Drop(telemetry.DropPathMixer),
			allyBroadcast: func(pkt []byte) {
				allySession.BroadcastFromGuild(guestGuildID, pkt)
			},
		})
		sourceSlots = append(sourceSlots, slot)
	}

	gm := p.GuestGM
	r := router.New(p.GuestGuildID, p.AllowFilter.RoleID(), p.VoiceProbe, sourceSlots, dests).
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
		GuildID: p.GuestGuildID,
		Cancel:  p.CancelFunc,
		Cleanup: func() {
			r.Close()
			speakerCleanup()
		},
		AllyCode:      p.Code,
		IsGuest:       true,
		Speakers:      p.Setup.Speakers,
		ChannelMixers: mixerPausers,
		AllowFilter:   p.AllowFilter,
		AutoRouter:    r,
	}

	// relayInputs is populated inside start() and closed by cleanup(); safe
	// because the teardown goroutine in JoinSession only runs after start().
	var relayInputs []chan<- []byte
	start := func() {
		relayInputs = RegisterRelayInputs(ctx, p.GuestGM, p.AllySession, destinations, channelMixers)
		StartChannelMixers(ctx, p.GuestGM, destinations, channelMixers)
		StartGuestRelayBroadcast(ctx, relayMixer, p.AllySession, p.GuestGuildID)
		r.Recompute()
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	cleanup := func() {
		for _, ch := range relayInputs {
			close(ch)
		}
	}
	return session, start, cleanup, nil
}
