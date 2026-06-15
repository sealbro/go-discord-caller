package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// relayDestID is the synthetic destination channelID used to model the ally
// relay mixer as a router.DestSlot in the router topology. Discord snowflakes are
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

	ownerSlot := &router.SourceSlot{
		ID:        p.ownerBotID,
		ChannelID: p.ov.ChannelID(),
		Handle:    p.ownerHandle,
	}
	dests := make([]*router.DestSlot, 0, len(destinations)+1)
	for _, dest := range destinations {
		ds := &router.DestSlot{
			ChannelID: dest.channelID,
			Mixer:     channelMixers[dest.channelID],
			ChOuts:    dest.outs,
			Sources:   []*router.SourceSlot{ownerSlot},
		}
		dests = append(dests, ds)
	}
	relaySlot := &router.DestSlot{
		ChannelID: relayDestID,
		Mixer:     relayMixer,
		ChOuts:    nil,
		Sources:   []*router.SourceSlot{ownerSlot},
	}
	dests = append(dests, relaySlot)
	ownerSlot.Feeds = dests
	allySession := p.allySession
	guildID := p.guildID
	ownerSlot.BuildInstall = routerInstallBuilder(installBuildOpts{
		src:        ownerSlot,
		dropDirect: p.gm.Drop(telemetry.DropPathDirect),
		dropMixer:  p.gm.Drop(telemetry.DropPathMixer),
		allyBroadcast: func(pkt []byte) {
			allySession.BroadcastFromGuild(guildID, pkt)
		},
	})

	// Construct router but do NOT seed it from any caller count yet — the
	// initial Recompute fires after commitSession so the disgo cache has
	// the post-join voice states.
	gm := p.gm
	r := router.New(p.guildID, p.allowFilter.RoleID(), p.voiceProbe, []*router.SourceSlot{ownerSlot}, dests).
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
			// r.Close releases mixer inputs allocated by the latest
			// install + stops pending debounce timers. Must run before the
			// speaker cleanup so any in-flight teardown doesn't race with
			// the mixer goroutines being closed.
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
		startChannelMixers(ctx, p.gm, destinations, channelMixers)
		startRelayBroadcast(ctx, p.gm, relayMixer, p.allySession, p.ownerCleanup)
		// Initial route: the cache is now up to date with voice states for
		// every member in the owner channel (prefetchChannelMembers and the
		// pre-StartVoiceRaid cache warm ran already), so Recompute can
		// compute caller counts directly. Router takes over install on the
		// owner FanoutHandle from here.
		r.Recompute()
	}
	return session, start, nil
}

// installBuildOpts parameterises the unified routerInstallBuilder. The three
// boolean-like fields cover the three topology shapes that exist today:
//
//   - mix-minus / unicast (OneCaller, GuildCaller, AllyCaller, guest star):
//     allyBroadcast set, rawTargets nil → copy uses union of feeds.chOuts +
//     OpusCallback; mix uses per-user SourceBuffers.
//
//   - star owner (OneManyGuildCaller / OneManyAllyCaller host owner):
//     allyBroadcast set AND rawTargets non-nil → copy uses fixed speaker
//     chOuts + OpusCallback; mix keeps the same fixed OpusTargets *and*
//     adds per-user relay SourceBuffers (since speakers receive raw Opus
//     in both modes).
//
//   - star speaker (OneManyGuildCaller / OneManyAllyCaller speaker source):
//     allyBroadcast nil → copy uses union of feeds.chOuts (no OpusCallback,
//     no DropOpus); mix uses per-user SourceBuffers. Star speakers never
//     relay to ally guests.
type installBuildOpts struct {
	src        *router.SourceSlot
	dropDirect func()
	dropMixer  func()
	// allyBroadcast, when non-nil, is set as OpusCallback in copy mode and
	// triggers DropOpus = dropDirect. In mix mode the relay mixer
	// SourceBuffer handles the broadcast instead, so this field is unused
	// there.
	allyBroadcast func([]byte)
	// rawTargets, when non-nil, override the union-of-feeds.chOuts in copy
	// mode AND stay installed in mix mode alongside SourceTargets. Used by
	// the star-owner closure to keep raw Opus flowing to speaker chOuts
	// regardless of mode (since star has no per-speaker mixer).
	rawTargets []chan<- []byte
}

// routerInstallBuilder returns the buildInstall closure for a source. One
// builder handles every topology — pipelines configure behaviour via opts
// rather than each pipeline having its own near-duplicate closure. See
// installBuildOpts for the per-shape parameters.
func routerInstallBuilder(opts installBuildOpts) func(router.RouteMode, []router.UserBinding) (opus.FanoutInstall, func()) {
	return func(mode router.RouteMode, users []router.UserBinding) (opus.FanoutInstall, func()) {
		switch mode {
		case router.RouteOff:
			return opus.FanoutInstall{}, func() {}
		case router.RouteCopy:
			outs := opts.rawTargets
			if outs == nil {
				for _, d := range opts.src.Feeds {
					outs = append(outs, d.ChOuts...)
				}
			}
			spec := opus.FanoutInstall{OpusTargets: outs}
			if opts.allyBroadcast != nil {
				spec.OpusCallback = opts.allyBroadcast
				spec.DropOpus = opts.dropDirect
			}
			return spec, func() {}
		case router.RouteMix:
			spec, teardown := buildPerUserMixSpec(opts.src, users, opts.dropMixer)
			if opts.rawTargets != nil {
				spec.OpusTargets = opts.rawTargets
				spec.DropOpus = opts.dropDirect
			}
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
func buildPerUserMixSpec(src *router.SourceSlot, users []router.UserBinding, dropMixer func()) (opus.FanoutInstall, func()) {
	sourceTargets := make(map[snowflake.ID][]*opus.SourceBuffer, len(users))
	var removals []mixerRef
	var sbs []*opus.SourceBuffer
	for _, u := range users {
		var userSBs []*opus.SourceBuffer
		for _, d := range src.Feeds {
			if d.Mixer == nil {
				continue
			}
			sb := opus.NewSourceBuffer(dropMixer)
			if err := d.Mixer.AddInput(u.SynthID, sb); err != nil {
				// Silent skip would mean this user's audio never reaches
				// this destination — surface it so the operator can
				// correlate with the mixer-internal error.
				slog.Warn("auto-route: mixer.AddInput failed; skipping user→dest binding",
					slog.String("sourceID", src.ID.String()),
					slog.String("destID", d.ChannelID.String()),
					slog.String("userID", u.UserID.String()),
					slog.Any("err", err))
				continue
			}
			userSBs = append(userSBs, sb)
			sbs = append(sbs, sb)
			removals = append(removals, mixerRef{mx: d.Mixer, id: u.SynthID})
		}
		if len(userSBs) > 0 {
			sourceTargets[u.UserID] = userSBs
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
