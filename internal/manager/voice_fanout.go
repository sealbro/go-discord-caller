package manager

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// relayBridgeDrainThreshold is the number of queued Opus packets in the relay
// input channel above which the bridge goroutine drains to the latest.
// 3 frames × 20 ms = 60 ms of tolerated relay jitter before drain kicks in.
const relayBridgeDrainThreshold = 3

// mixerRef pairs a mixer with the source ID registered in it, so the fanout
// goroutine can call RemoveInput when the source channel is exhausted.
type mixerRef struct {
	mx *opus.Mixer
	id snowflake.ID
}

// tryAddMixerInput creates a buffered frame channel and registers it as an input
// on mx for id. On success the channel and a removal entry are appended to
// *fanTargets and *removals. On failure a warning is logged with label as the
// component prefix (e.g. "relay mixer", "channel mixer").
func tryAddMixerInput(ctx context.Context, mx *opus.Mixer, id snowflake.ID, label string, fanTargets *[]chan opus.Frame, removals *[]mixerRef) {
	ch := make(chan opus.Frame, audioChanBuf)
	if err := mx.AddInput(id, ch); err != nil {
		slog.WarnContext(ctx, label+": failed to add input", slog.Any("err", err))
		return
	}
	*fanTargets = append(*fanTargets, ch)
	*removals = append(*removals, mixerRef{mx, id})
}

// endSession runs the common host session teardown: invokes ownerCleanup, records
// the stop metric, ends the tracing span, and logs the session end.
// Intended to be called as a deferred statement inside session goroutines.
func endSession(ctx context.Context, ownerCleanup func(), gm telemetry.GuildMetrics) {
	ownerCleanup()
	gm.SessionStopped()
	trace.SpanFromContext(ctx).End()
	slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", gm.GuildID().String()))
}

// installFanoutSource installs frame targets on src.handle so that every Opus
// packet received by the source's VoiceReceiver is decoded inline (in
// ReceiveOpusFrame, on disgo's UDP read goroutine) and distributed to every
// mixer input channel in frameTargets — eliminating the per-source decode
// goroutine and the buffered-chan hop that previously sat between the
// receiver and the mixers.
//
// On handle close (session-end teardown) OnClose calls RemoveInput on every
// registered mixer, mirroring the deferred cleanup in the old goroutine path.
//
// READ-ONLY CONTRACT: the Opus bytes referenced by each Frame are shared
// across all FrameTargets. No consumer may mutate them. The mixer copies them
// before forwarding to its output channel (see Mixer.tick single-source path),
// so downstream consumers always get their own slice.
func installFanoutSource(handle *opus.FanoutHandle, frameTargets []chan opus.Frame, removals []mixerRef, drop func()) {
	if handle == nil {
		slog.Error("fanout: source has no handle, frames will not be dispatched")
		return
	}
	targets := make([]chan<- opus.Frame, len(frameTargets))
	for i, t := range frameTargets {
		targets[i] = t
	}
	handle.Install(opus.FanoutInstall{
		FrameTargets: targets,
		OnClose: func() {
			for _, r := range removals {
				r.mx.RemoveInput(r.id)
			}
		},
		DropFrame: drop,
	})
}

// wireFanout starts a goroutine per source that decodes each incoming Opus packet
// exactly once and distributes the resulting PCM to all relevant mixer inputs.
// Decoding once (vs once per mixer) cuts decode operations from sources×mixers to
// sources. The relay mixer receives every source; per-channel mixers skip the source
// from their own channel (mix-minus).
// Each goroutine calls RemoveInput on every mixer it registered when it exits,
// so stale entries are not retained for the lifetime of the session.
func wireFanout(ctx context.Context, gm telemetry.GuildMetrics, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer) {
	drop := gm.Drop(telemetry.DropPathMixer)
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		tryAddMixerInput(ctx, relayMixer, src.id, "relay mixer", &fanTargets, &removals)

		for _, dest := range dests {
			if dest.channelID == src.channelID {
				continue // mix-minus: don't relay audio back to its origin channel
			}
			tryAddMixerInput(ctx, chanMixers[dest.channelID], src.id, "channel mixer", &fanTargets, &removals)
		}

		installFanoutSource(src.handle, fanTargets, removals, drop)
	}
}

// wireFanoutOneMany implements a star-topology fanout where the owner channel is
// the central hub. The owner source fans out to all destination channel mixers
// (mix-minus), but speaker sources fan out ONLY to the owner's channel mixer —
// speakers cannot hear each other.
//
// ownerChannelID identifies the hub channel. When 0 (guest star mode), ALL
// sources go to the relay mixer only — no local channel-to-channel routing.
// The guest's channel mixers receive audio solely via registerRelayInputs
// (the host's relay), ensuring guest speakers hear only the host owner.
func wireFanoutOneMany(ctx context.Context, gm telemetry.GuildMetrics, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer, ownerChannelID snowflake.ID) {
	drop := gm.Drop(telemetry.DropPathMixer)
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		// All sources always feed the relay mixer.
		tryAddMixerInput(ctx, relayMixer, src.id, "relay mixer", &fanTargets, &removals)

		if ownerChannelID != 0 {
			if src.channelID == ownerChannelID {
				// Owner source → all channel mixers except its own (standard mix-minus).
				for _, dest := range dests {
					if dest.channelID == src.channelID {
						continue
					}
					tryAddMixerInput(ctx, chanMixers[dest.channelID], src.id, "channel mixer", &fanTargets, &removals)
				}
			} else {
				// Speaker source → owner channel mixer ONLY (star spoke → hub).
				if ownerMixer, ok := chanMixers[ownerChannelID]; ok {
					tryAddMixerInput(ctx, ownerMixer, src.id, "channel mixer", &fanTargets, &removals)
				}
			}
		}
		// When ownerChannelID == 0 (guest star), sources go to relay only.

		installFanoutSource(src.handle, fanTargets, removals, drop)
	}
}

// installFanoutOwnerStar installs the owner-star fanout on handle: raw Opus
// bytes are forwarded directly to every speaker chOut in directOuts (no
// re-encode — owner is the only source feeding those channels), and the
// inline-decoded Frame is delivered into the relay mixer so guests receive
// the owner's audio. Replaces the per-source decode goroutine.
//
// On handle close (session end) OnClose detaches the relay mixer input and
// closes every directOuts channel — equivalent to the old goroutine's defer.
func installFanoutOwnerStar(handle *opus.FanoutHandle, directOuts []chan<- []byte, relayMixCh chan opus.Frame, relayMixer *opus.Mixer, srcID snowflake.ID, dropDirect, dropRelay func()) {
	if handle == nil {
		slog.Error("owner star fanout: source has no handle, frames will not be dispatched")
		return
	}
	handle.Install(opus.FanoutInstall{
		OpusTargets:  directOuts,
		FrameTargets: []chan<- opus.Frame{relayMixCh},
		OnClose: func() {
			relayMixer.RemoveInput(srcID)
			for _, out := range directOuts {
				close(out)
			}
		},
		DropOpus:  dropDirect,
		DropFrame: dropRelay,
	})
}

// wireFanoutOneManyDirect implements host star-topology fanout with direct speaker delivery.
// The owner source sends raw Opus to directSpeakerOuts (no decode) and a decoded Frame
// to the relay mixer. Each speaker source decodes once and sends to the hub mixer + relay.
// No N-1 channel mixers are created; speaker channels receive audio directly from the owner.
func wireFanoutOneManyDirect(ctx context.Context, gm telemetry.GuildMetrics, sources []sourceEntry, ownerChannelID snowflake.ID, directSpeakerOuts []chan<- []byte, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer) {
	drop := gm.Drop(telemetry.DropPathMixer)
	dropDirect := gm.Drop(telemetry.DropPathOwnerStarDirect)
	dropRelay := gm.Drop(telemetry.DropPathOwnerStarRelay)
	for _, src := range sources {
		if src.channelID == ownerChannelID {
			relayCh := make(chan opus.Frame, audioChanBuf)
			if err := relayMixer.AddInput(src.id, relayCh); err != nil {
				slog.WarnContext(ctx, "relay mixer: failed to add owner input", slog.Any("err", err))
				continue
			}
			installFanoutOwnerStar(src.handle, directSpeakerOuts, relayCh, relayMixer, src.id, dropDirect, dropRelay)
		} else {
			// Speaker source: decode once → hub mixer only (star spoke → hub).
			// Speaker audio is NOT relayed to guests — only the owner/caller's audio is.
			var fanTargets []chan opus.Frame
			var removals []mixerRef

			if hubMixer, ok := chanMixers[ownerChannelID]; ok {
				tryAddMixerInput(ctx, hubMixer, src.id, "hub mixer", &fanTargets, &removals)
			}

			installFanoutSource(src.handle, fanTargets, removals, drop)
		}
	}
}

// startFanoutDirect is the bypass path for RaidModeOneCaller.
// Raw Opus packets are read from in and copied to every speaker output channel
// and the relay session without any PCM decode/encode step.
// The goroutine closes all outs when it exits so VoiceProviders shut down cleanly.
func startFanoutDirect(ctx context.Context, gm telemetry.GuildMetrics, in <-chan []byte, outs []chan<- []byte, session *ally.Session) {
	drop := gm.Drop(telemetry.DropPathDirect)
	guildID := gm.GuildID()
	go func() {
		defer func() {
			for _, out := range outs {
				close(out)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-in:
				if !ok {
					return
				}
				if len(pkt) == 0 {
					continue
				}
				// Each consumer (VoiceProvider) independently calls PutEncodedFrame
				// on the buffer it receives. Sending the same pkt to N consumers
				// would cause N pool-returns for one allocation. Copy per consumer.
				for _, out := range outs {
					buf := opus.CopyOpusFrame(pkt)
					select {
					case out <- buf:
					default:
						opus.PutEncodedFrame(buf)
						drop()
					}
				}
				// session.BroadcastFromGuild takes ownership of pkt: it copies per
				// relay channel and returns the original to the pool itself.
				session.BroadcastFromGuild(guildID, pkt)
			}
		}
	}()
}

// startChannelMixers runs each per-channel mixer with a sink that distributes
// produced frames directly to every speaker output channel for the destination.
// Removing the per-mixer forwarder goroutine cuts one channel hop and one
// scheduler wake-up per produced frame. destOuts are closed after Run returns
// so VoiceProvider goroutines shut down cleanly; this goroutine is the sole
// writer to destOuts after the sink stops being invoked (Run has returned).
func startChannelMixers(ctx context.Context, gm telemetry.GuildMetrics, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) {
	drop := gm.Drop(telemetry.DropPathChannelMixer)
	for _, dest := range dests {
		mx := chanMixers[dest.channelID]
		destOuts := dest.outs
		mx.SetSink(func(pkt []byte) {
			for _, out := range destOuts {
				select {
				case out <- pkt:
				default:
					drop()
				}
			}
		})
		go func(mx *opus.Mixer, destOuts []chan<- []byte) {
			mx.Run(ctx)
			for _, out := range destOuts {
				close(out)
			}
		}(mx, destOuts)
		go opus.NewDrainWatcher(mx, opus.DrainIdleTimeout).Run(ctx)
	}
}

// startRelayBroadcast runs the relay mixer with a sink that broadcasts each
// produced frame directly to all guest guilds. endSession runs after Run
// returns, by which time tick (and therefore the sink) is no longer invoked,
// so cleanup is ordered after the last broadcast.
func startRelayBroadcast(ctx context.Context, gm telemetry.GuildMetrics, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func()) {
	guildID := gm.GuildID()
	relayMixer.SetSink(func(pkt []byte) {
		relaySession.BroadcastFromGuild(guildID, pkt)
	})
	go func() {
		defer endSession(ctx, ownerCleanup, gm)
		relayMixer.Run(ctx)
	}()
}

// startDirectSessionCleanup waits for ctx cancellation then runs teardown.
func startDirectSessionCleanup(ctx context.Context, gm telemetry.GuildMetrics, ownerCleanup func()) {
	go func() {
		defer endSession(ctx, ownerCleanup, gm)
		<-ctx.Done()
	}()
}

// startGuestRelayBroadcast runs the guest relay mixer with a sink that
// broadcasts each produced frame directly to all OTHER guilds via
// BroadcastFromGuild, excluding the guest itself.
func startGuestRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, session *ally.Session, guestGuildID snowflake.ID) {
	relayMixer.SetSink(func(pkt []byte) {
		session.BroadcastFromGuild(guestGuildID, pkt)
	})
	go relayMixer.Run(ctx)
}

// registerRelayInputs wires a guild as a relay receiver in the ally session.
// A single Opus input channel is registered with the session. One bridge
// goroutine decodes each incoming packet exactly once and fans the resulting
// opus.Frame out to every destination channel mixer — the relay equivalent of
// what installFanoutSource sets up for local VoiceReceiver-backed sources
// (the bridge stays as a goroutine because relay packets arrive on a chan
// from another guild rather than via inline ReceiveOpusFrame). Returns the
// single Opus input channel so the caller can close it on teardown (closing
// triggers bridge goroutine exit, which then closes all downstream frame
// channels).
func registerRelayInputs(ctx context.Context, gm telemetry.GuildMetrics, session *ally.Session, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) []chan<- []byte {
	// Register one frame output channel per destination mixer.
	frameOuts := make([]chan opus.Frame, 0, len(dests))
	for _, dest := range dests {
		frameOut := make(chan opus.Frame, audioChanBuf)
		if err := chanMixers[dest.channelID].AddInput(relayInputID, frameOut); err != nil {
			slog.Warn("relay: failed to add input to channel mixer",
				slog.String("channelID", dest.channelID.String()),
				slog.Any("err", err))
			close(frameOut)
			continue
		}
		frameOuts = append(frameOuts, frameOut)
	}
	if len(frameOuts) == 0 {
		return nil
	}

	// Single Opus input channel shared across all destination mixers.
	// Bridge: decode once, fan Frame out to every registered mixer input.
	// Both PCM and original Opus bytes are forwarded so the mixer can apply
	// the single-source passthrough optimisation when only one source is active.
	// Exits when relayOpusIn is closed (session teardown closes it via toClose).
	drop := gm.Drop(telemetry.DropPathRelayBridge)
	relayOpusIn := make(chan []byte, audioChanBuf)
	go func(ctx context.Context, in <-chan []byte, outs []chan opus.Frame) {
		defer func() {
			for _, out := range outs {
				close(out)
			}
		}()
		dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
		if err != nil {
			slog.Error("relay bridge: failed to create decoder", slog.Any("err", err))
			return
		}
		scratch := make([]int16, opus.MixerPCMBuf)
		for pkt := range in {
			// Bleed-off drain: when the backlog exceeds the threshold, skip
			// just one extra packet this iteration (at most one extra 20 ms
			// gap per iteration). Repeated iterations converge on the live
			// edge without producing a single audible burst gap, which a
			// full drain-to-latest would do under load.
			if len(in) > relayBridgeDrainThreshold {
				select {
				case newer, ok := <-in:
					if !ok {
						return
					}
					drop()
					pkt = newer
				default:
				}
			}
			if len(pkt) == 0 {
				continue
			}
			n, err := dec.Decode(pkt, scratch)
			if err != nil {
				slog.Debug("relay bridge: decode failed", slog.Any("err", err))
				continue
			}
			now := time.Now()
			for _, out := range outs {
				pcm := opus.GetPCM()[:n*opus.MixerChannels]
				copy(pcm, scratch[:n*opus.MixerChannels])
				// Each mixer may do single-source passthrough, forwarding Frame.Opus
				// directly to its sink. If multiple mixers share the same Opus slice
				// each VoiceProvider would call PutEncodedFrame on the same backing
				// array. Copy per output so every consumer owns its buffer.
				opusCopy := opus.CopyOpusFrame(pkt)
				select {
				case out <- opus.Frame{PCM: pcm, Opus: opusCopy, CreatedAt: now}:
				default:
					opus.PutPCM(pcm)
					opus.PutEncodedFrame(opusCopy)
					drop()
				}
			}
		}
	}(ctx, relayOpusIn, frameOuts)

	session.AddGuild(gm.GuildID(), []chan<- []byte{relayOpusIn})
	return []chan<- []byte{relayOpusIn}
}
