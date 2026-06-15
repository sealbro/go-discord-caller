package pipeline

import (
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// installBuildOpts parameterises the unified routerInstallBuilder. The three
// boolean-like fields cover the three topology shapes that exist today:
//
//   - mix-minus / unicast (OneCaller, GuildCaller, AllyCaller, guest star):
//     allyBroadcast set, rawTargets nil → copy uses union of feeds.ChOuts +
//     OpusCallback; mix uses per-user SourceBuffers.
//
//   - star owner (OneManyGuildCaller / OneManyAllyCaller host owner):
//     allyBroadcast set AND rawTargets non-nil → copy uses fixed speaker
//     chOuts + OpusCallback; mix keeps the same fixed OpusTargets *and*
//     adds per-user relay SourceBuffers (since speakers receive raw Opus
//     in both modes).
//
//   - star speaker (OneManyGuildCaller / OneManyAllyCaller speaker source):
//     allyBroadcast nil → copy uses union of feeds.ChOuts (no OpusCallback,
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
	// rawTargets, when non-nil, override the union-of-feeds.ChOuts in copy
	// mode AND stay installed in mix mode alongside SourceTargets. Used by
	// the star-owner closure to keep raw Opus flowing to speaker chOuts
	// regardless of mode (since star has no per-speaker mixer).
	rawTargets []chan<- []byte
}

// routerInstallBuilder returns the BuildInstall closure for a source. One
// builder handles every topology — pipelines configure behaviour via opts
// rather than each pipeline having its own near-duplicate closure. See
// installBuildOpts for the per-shape parameters.
func routerInstallBuilder(opts installBuildOpts) router.BuildInstallFunc {
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
