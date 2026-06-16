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

// OneCallerPipeline handles RaidModeOneCaller: single owner source feeds N
// speaker channels. The router decides whether the owner runs in copy mode
// (raw Opus passthrough + OpusCallback to ally) or mix mode (decoded into the
// per-channel and relay mixers) based on the live caller count in the owner
// channel.
//
// Replaces directPipeline. Always-on mixer graph: per-speaker-channel mixers
// and the relay mixer are created up-front and started paused. Initial
// Recompute installs the right mode without a mid-session "build a mixer"
// branch.
type OneCallerPipeline struct{}

func (OneCallerPipeline) Build(ctx context.Context, p Params) (*guild.Session, func(), error) {
	destinations := BuildDestinations(p.Setup.Joined)

	// Per-speaker-channel mixers. Each writes its sink output to that
	// channel's speaker Outs.
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.GM.Opus)
		if err != nil {
			return nil, nil, fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.ChannelID] = mx
	}
	// Relay mixer: produces the host's broadcast for ally guests when the
	// owner is in mix mode. In copy mode it stays paused and the ally feed
	// comes from the source's OpusCallback instead.
	relayMixer, err := opus.NewMixer(p.GM.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	ownerSlot := &router.SourceSlot{
		ID:        p.OwnerBotID,
		ChannelID: p.OV.ChannelID(),
		Handle:    p.OwnerHandle,
	}
	dests := make([]*router.DestSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		ds := &router.DestSlot{
			ChannelID: dest.ChannelID,
			Mixer:     channelMixers[dest.ChannelID],
			ChOuts:    dest.Outs,
			Sources:   []*router.SourceSlot{ownerSlot},
		}
		dests = append(dests, ds)
	}
	relaySlot := &router.DestSlot{
		ChannelID: RelayDestID,
		Mixer:     relayMixer,
		ChOuts:    nil,
		Sources:   []*router.SourceSlot{ownerSlot},
	}
	dests = append(dests, relaySlot)
	ownerSlot.Feeds = dests
	allySession := p.AllySession
	guildID := p.GuildID
	ownerSlot.BuildInstall = routerInstallBuilder(installBuildOpts{
		src:        ownerSlot,
		dropDirect: p.GM.Drop(telemetry.DropPathDirect),
		dropMixer:  p.GM.Drop(telemetry.DropPathMixer),
		allyBroadcast: func(pkt []byte) {
			allySession.BroadcastFromGuild(guildID, pkt)
		},
	})

	// Construct router but do NOT seed it from any caller count yet — the
	// initial Recompute fires after commitSession so the disgo cache has
	// the post-join voice states.
	gm := p.GM
	r := router.New(p.GuildID, p.AllowFilter.RoleID(), p.VoiceProbe, []*router.SourceSlot{ownerSlot}, dests).
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
			// r.Close releases mixer inputs allocated by the latest
			// install + stops pending debounce timers. Must run before the
			// speaker cleanup so any in-flight teardown doesn't race with
			// the mixer goroutines being closed.
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
		StartChannelMixers(ctx, p.GM, destinations, channelMixers)
		StartRelayBroadcast(ctx, p.GM, relayMixer, p.AllySession, p.OwnerCleanup)
		// Initial route: the cache is now up to date with voice states for
		// every member in the owner channel (prefetchChannelMembers and the
		// pre-StartVoiceRaid cache warm ran already), so Recompute can
		// compute caller counts directly. Router takes over install on the
		// owner FanoutHandle from here.
		r.Recompute()
		// VOICE_STATE_UPDATE events for users already in the owner channel
		// may still be in flight via the gateway. Schedule a single
		// followup Recompute to catch them before the user-perceived delay
		// becomes noticeable.
		r.ScheduleRecompute(500 * time.Millisecond)
	}
	return session, start, nil
}
