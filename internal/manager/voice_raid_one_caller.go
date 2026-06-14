package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// relayDestID is the synthetic destination channelID used to model the ally
// relay mixer as a destSlot in the router topology. Discord snowflakes are
// epoch-based with a minimum value around 4×10¹⁰ (Discord's epoch is
// 2015-01-01), so any small integer is safely below the real-channelID
// range. relayInputID = 1 is already used for the host-side guest-input
// source, so 2 is the next safe choice.
const relayDestID snowflake.ID = 2

// oneCallerPipeline handles RaidModeOneCaller: single owner source feeds N
// speaker channels. The router decides whether the owner runs in copy mode
// (raw Opus passthrough + OpusCallback to ally) or mix mode (decoded into the
// per-channel and relay mixers) based on the live caller count in the owner
// channel.
//
// Replaces directPipeline. Always-on mixer graph: per-speaker-channel mixers
// and the relay mixer are created up-front and started paused. Initial
// Recompute installs the right mode without a mid-session "build a mixer"
// branch.
type oneCallerPipeline struct{}

func (oneCallerPipeline) build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error) {
	destinations := buildDestinations(p.setup.joined)

	// Per-speaker-channel mixers. Each writes its sink output to that
	// channel's speaker chOuts.
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(p.gm.Opus)
		if err != nil {
			return nil, nil, fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}
	// Relay mixer: produces the host's broadcast for ally guests when the
	// owner is in mix mode. In copy mode it stays paused and the ally feed
	// comes from the source's OpusCallback instead.
	relayMixer, err := opus.NewMixer(p.gm.Opus)
	if err != nil {
		return nil, nil, fmt.Errorf("create relay mixer: %w", err)
	}

	ownerSlot := &sourceSlot{
		id:        p.ownerBotID,
		channelID: p.ov.ChannelID(),
		handle:    p.ownerHandle,
	}
	dests := make([]*destSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		ds := &destSlot{
			channelID: dest.channelID,
			mixer:     channelMixers[dest.channelID],
			chOuts:    dest.outs,
			sources:   []*sourceSlot{ownerSlot},
		}
		dests = append(dests, ds)
	}
	relaySlot := &destSlot{
		channelID: relayDestID,
		mixer:     relayMixer,
		chOuts:    nil,
		sources:   []*sourceSlot{ownerSlot},
	}
	dests = append(dests, relaySlot)
	ownerSlot.feeds = dests
	ownerSlot.buildInstall = oneCallerInstallBuilder(p.gm, p.guildID, ownerSlot, p.allySession)

	// Construct router but do NOT seed it from any caller count yet — the
	// initial Recompute fires after commitSession so the disgo cache has
	// the post-join voice states.
	router := newSourceRouter(p.guildID, p.allowFilter.RoleID(), p.voiceProbe, []*sourceSlot{ownerSlot}, dests)

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
			// router.Close releases mixer inputs allocated by the latest
			// install + stops pending debounce timers. Must run before the
			// speaker cleanup so any in-flight teardown doesn't race with
			// the mixer goroutines being closed.
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
		startChannelMixers(ctx, p.gm, destinations, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
		// Initial route: the cache is now up to date with voice states for
		// every member in the owner channel (prefetchChannelMembers and the
		// pre-StartVoiceRaid cache warm ran already), so Recompute can
		// compute caller counts directly. Router takes over install on the
		// owner FanoutHandle from here.
		router.Recompute()
	}
	return session, start, nil
}

// oneCallerInstallBuilder returns the buildInstall closure for the OneCaller
// owner source. Closes over the ally session + guild metrics so each
// transition has the topology details to install.
func oneCallerInstallBuilder(gm telemetry.GuildMetrics, guildID snowflake.ID, owner *sourceSlot, allySession *ally.Session) func(routeMode, []userBinding) (opus.FanoutInstall, func()) {
	dropDirect := gm.Drop(telemetry.DropPathDirect)
	dropMixer := gm.Drop(telemetry.DropPathMixer)
	return func(mode routeMode, users []userBinding) (opus.FanoutInstall, func()) {
		switch mode {
		case routeOff:
			return opus.FanoutInstall{}, func() {}
		case routeCopy:
			// Speaker channels get raw Opus directly; ally session gets a
			// parallel copy via OpusCallback. Relay mixer stays paused so
			// its sink does not produce a duplicate broadcast.
			var outs []chan<- []byte
			for _, d := range owner.feeds {
				outs = append(outs, d.chOuts...)
			}
			spec := opus.FanoutInstall{
				OpusTargets: outs,
				OpusCallback: func(pkt []byte) {
					allySession.BroadcastFromGuild(guildID, pkt)
				},
				DropOpus: dropDirect,
			}
			return spec, func() {}
		case routeMix:
			spec, teardown := buildPerUserMixSpec(owner, users, dropMixer)
			return spec, teardown
		}
		return opus.FanoutInstall{}, func() {}
	}
}

// buildPerUserMixSpec allocates one SourceBuffer per (user, destination
// mixer) pair, registers each under its synth ID with the destination mixer,
// and returns a FanoutInstall keyed by userID plus a teardown closure that
// detaches and drains them. Shared by every mix-mode closure that has no
// extra topology rules (OneCaller / GuildCaller / star speaker source).
func buildPerUserMixSpec(src *sourceSlot, users []userBinding, dropMixer func()) (opus.FanoutInstall, func()) {
	sourceTargets := make(map[snowflake.ID][]*opus.SourceBuffer, len(users))
	var removals []mixerRef
	var sbs []*opus.SourceBuffer
	for _, u := range users {
		var userSBs []*opus.SourceBuffer
		for _, d := range src.feeds {
			if d.mixer == nil {
				continue
			}
			sb := opus.NewSourceBuffer(dropMixer)
			if err := d.mixer.AddInput(u.synthID, sb); err != nil {
				// Silent skip would mean this user's audio never reaches
				// this destination — surface it so the operator can
				// correlate with the mixer-internal error.
				slog.Warn("auto-route: mixer.AddInput failed; skipping user→dest binding",
					slog.String("sourceID", src.id.String()),
					slog.String("destID", d.channelID.String()),
					slog.String("userID", u.userID.String()),
					slog.Any("err", err))
				continue
			}
			userSBs = append(userSBs, sb)
			sbs = append(sbs, sb)
			removals = append(removals, mixerRef{mx: d.mixer, id: u.synthID})
		}
		if len(userSBs) > 0 {
			sourceTargets[u.userID] = userSBs
		}
	}
	teardown := func() {
		for _, r := range removals {
			r.mx.RemoveInput(r.id)
		}
		for _, sb := range sbs {
			sb.Drain()
		}
	}
	return opus.FanoutInstall{SourceTargets: sourceTargets}, teardown
}
