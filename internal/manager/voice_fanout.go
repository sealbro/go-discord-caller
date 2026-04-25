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

// runFanoutSource is the shared goroutine body for wireFanout and wireFanoutOneMany.
// It decodes each incoming Opus packet exactly once and distributes the resulting
// Frame to all mixer input channels in targets. When the source channel closes or
// ctx is cancelled, it calls RemoveInput on every mixer it registered and exits.
//
// READ-ONLY CONTRACT: pkt is shared across every Frame sent to targets.
// No consumer may mutate pkt. The mixer copies it before forwarding
// to its output channel (see Mixer.tick single-source path), so
// downstream consumers always get their own slice.
func runFanoutSource(ctx context.Context, guildID snowflake.ID, in <-chan []byte, targets []chan opus.Frame, removals []mixerRef, sm *telemetry.SessionMetrics) {
	defer func() {
		for _, r := range removals {
			r.mx.RemoveInput(r.id)
		}
	}()
	dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
	if err != nil {
		slog.ErrorContext(ctx, "fanout: failed to create decoder", slog.Any("err", err))
		return
	}
	// Pre-compute drop attribute once to avoid per-frame allocations on the hot path.
	dropOpt := sm.DropOption(guildID, "mixer")
	scratch := make([]int16, opus.MixerPCMBuf)
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-in:
			if !ok {
				return
			}
			if len(pkt) == 0 {
				continue // DTX silence — nothing to decode or distribute
			}
			n, err := dec.Decode(pkt, scratch)
			if err != nil {
				slog.Debug("fanout: decode failed", slog.Any("err", err))
				continue
			}
			now := time.Now()
			for _, t := range targets {
				pcm := opus.GetPCM()[:n*opus.MixerChannels]
				copy(pcm, scratch[:n*opus.MixerChannels])
				select {
				case t <- opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: now}:
				default:
					opus.PutPCM(pcm) // channel full — drop frame
					sm.FrameDropped(ctx, dropOpt)
				}
			}
		}
	}
}

// wireFanout starts a goroutine per source that decodes each incoming Opus packet
// exactly once and distributes the resulting PCM to all relevant mixer inputs.
// Decoding once (vs once per mixer) cuts decode operations from sources×mixers to
// sources. The relay mixer receives every source; per-channel mixers skip the source
// from their own channel (mix-minus).
// Each goroutine calls RemoveInput on every mixer it registered when it exits,
// so stale entries are not retained for the lifetime of the session.
func wireFanout(ctx context.Context, guildID snowflake.ID, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer, sm *telemetry.SessionMetrics) {
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		relayCh := make(chan opus.Frame, audioChanBuf)
		if err := relayMixer.AddInput(src.id, relayCh); err != nil {
			slog.WarnContext(ctx, "relay mixer: failed to add input", slog.Any("err", err))
		} else {
			fanTargets = append(fanTargets, relayCh)
			removals = append(removals, mixerRef{relayMixer, src.id})
		}

		for _, dest := range dests {
			if dest.channelID == src.channelID {
				continue // mix-minus: don't relay audio back to its origin channel
			}
			mixCh := make(chan opus.Frame, audioChanBuf)
			if err := chanMixers[dest.channelID].AddInput(src.id, mixCh); err != nil {
				slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
			} else {
				fanTargets = append(fanTargets, mixCh)
				removals = append(removals, mixerRef{chanMixers[dest.channelID], src.id})
			}
		}

		go runFanoutSource(ctx, guildID, src.ch, fanTargets, removals, sm)
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
func wireFanoutOneMany(ctx context.Context, guildID snowflake.ID, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer, ownerChannelID snowflake.ID, sm *telemetry.SessionMetrics) {
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		// All sources always feed the relay mixer.
		relayCh := make(chan opus.Frame, audioChanBuf)
		if err := relayMixer.AddInput(src.id, relayCh); err != nil {
			slog.WarnContext(ctx, "relay mixer: failed to add input", slog.Any("err", err))
		} else {
			fanTargets = append(fanTargets, relayCh)
			removals = append(removals, mixerRef{relayMixer, src.id})
		}

		if ownerChannelID != 0 {
			if src.channelID == ownerChannelID {
				// Owner source → all channel mixers except its own (standard mix-minus).
				for _, dest := range dests {
					if dest.channelID == src.channelID {
						continue
					}
					mixCh := make(chan opus.Frame, audioChanBuf)
					if err := chanMixers[dest.channelID].AddInput(src.id, mixCh); err != nil {
						slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
					} else {
						fanTargets = append(fanTargets, mixCh)
						removals = append(removals, mixerRef{chanMixers[dest.channelID], src.id})
					}
				}
			} else {
				// Speaker source → owner channel mixer ONLY (star spoke → hub).
				if ownerMixer, ok := chanMixers[ownerChannelID]; ok {
					mixCh := make(chan opus.Frame, audioChanBuf)
					if err := ownerMixer.AddInput(src.id, mixCh); err != nil {
						slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
					} else {
						fanTargets = append(fanTargets, mixCh)
						removals = append(removals, mixerRef{ownerMixer, src.id})
					}
				}
			}
		}
		// When ownerChannelID == 0 (guest star), sources go to relay only.

		go runFanoutSource(ctx, guildID, src.ch, fanTargets, removals, sm)
	}
}

// runFanoutOwnerStar handles the owner source in host star topology.
// Raw Opus bytes are sent directly to directOuts (speaker chOuts — no decode needed,
// they have exactly one source). Simultaneously, the packet is decoded once and the
// resulting Frame is forwarded to relayMixCh so the relay mixer can broadcast to guests.
// Closes directOuts when the source closes or ctx is cancelled.
func runFanoutOwnerStar(ctx context.Context, in <-chan []byte, directOuts []chan<- []byte, relayMixCh chan opus.Frame, relayMixer *opus.Mixer, srcID snowflake.ID) {
	defer func() {
		relayMixer.RemoveInput(srcID)
		for _, out := range directOuts {
			close(out)
		}
	}()
	dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
	if err != nil {
		slog.ErrorContext(ctx, "owner star fanout: failed to create decoder", slog.Any("err", err))
		return
	}
	scratch := make([]int16, opus.MixerPCMBuf)
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
			// Raw Opus → speaker channels (single source, no re-encode needed).
			for _, out := range directOuts {
				select {
				case out <- pkt:
				default:
				}
			}
			// Decode once for relay mixer.
			n, err := dec.Decode(pkt, scratch)
			if err != nil {
				slog.Debug("owner star fanout: decode failed", slog.Any("err", err))
				continue
			}
			pcm := opus.GetPCM()[:n*opus.MixerChannels]
			copy(pcm, scratch[:n*opus.MixerChannels])
			select {
			case relayMixCh <- opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: time.Now()}:
			default:
				opus.PutPCM(pcm)
			}
		}
	}
}

// wireFanoutOneManyDirect implements host star-topology fanout with direct speaker delivery.
// The owner source sends raw Opus to directSpeakerOuts (no decode) and a decoded Frame
// to the relay mixer. Each speaker source decodes once and sends to the hub mixer + relay.
// No N-1 channel mixers are created; speaker channels receive audio directly from the owner.
func wireFanoutOneManyDirect(ctx context.Context, guildID snowflake.ID, sources []sourceEntry, ownerChannelID snowflake.ID, directSpeakerOuts []chan<- []byte, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer, sm *telemetry.SessionMetrics) {
	for _, src := range sources {
		if src.channelID == ownerChannelID {
			relayCh := make(chan opus.Frame, audioChanBuf)
			if err := relayMixer.AddInput(src.id, relayCh); err != nil {
				slog.WarnContext(ctx, "relay mixer: failed to add owner input", slog.Any("err", err))
				continue
			}
			go runFanoutOwnerStar(ctx, src.ch, directSpeakerOuts, relayCh, relayMixer, src.id)
		} else {
			// Speaker source: decode once → hub mixer + relay (star spoke → hub).
			var fanTargets []chan opus.Frame
			var removals []mixerRef

			relayCh := make(chan opus.Frame, audioChanBuf)
			if err := relayMixer.AddInput(src.id, relayCh); err != nil {
				slog.WarnContext(ctx, "relay mixer: failed to add speaker input", slog.Any("err", err))
			} else {
				fanTargets = append(fanTargets, relayCh)
				removals = append(removals, mixerRef{relayMixer, src.id})
			}

			if hubMixer, ok := chanMixers[ownerChannelID]; ok {
				mixCh := make(chan opus.Frame, audioChanBuf)
				if err := hubMixer.AddInput(src.id, mixCh); err != nil {
					slog.WarnContext(ctx, "hub mixer: failed to add speaker input", slog.Any("err", err))
				} else {
					fanTargets = append(fanTargets, mixCh)
					removals = append(removals, mixerRef{hubMixer, src.id})
				}
			}

			go runFanoutSource(ctx, guildID, src.ch, fanTargets, removals, sm)
		}
	}
}

// wireFanoutDirect is the bypass path for RaidModeOneCaller.
// Raw Opus packets are read from in and copied to every speaker output channel
// and the relay session without any PCM decode/encode step.
// The goroutine closes all outs when it exits so VoiceProviders shut down cleanly.
func wireFanoutDirect(ctx context.Context, in <-chan []byte, outs []chan<- []byte, session *ally.Session, guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	// Pre-compute drop option once to avoid per-frame allocations on the hot path.
	dropOpt := sm.DropOption(guildID, "direct")
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
				for _, out := range outs {
					select {
					case out <- pkt:
					default:
						sm.FrameDropped(ctx, dropOpt)
					}
				}
				session.BroadcastFromGuild(guildID, pkt)
			}
		}
	}()
}

// startChannelMixers runs each per-channel mixer and forwards its output to all
// speaker output channels in that destination, closing them when the mixer stops.
// Closing destOuts signals VoiceProvider goroutines to shut down cleanly; it is
// safe because this goroutine is the sole writer after mixer.Run returns.
func startChannelMixers(ctx context.Context, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) {
	for _, dest := range dests {
		mx := chanMixers[dest.channelID]
		destOuts := dest.outs
		go mx.Run(ctx)
		go func(mx *opus.Mixer, destOuts []chan<- []byte) {
			for pkt := range mx.Output() {
				for _, out := range destOuts {
					select {
					case out <- pkt:
					default:
					}
				}
			}
			for _, out := range destOuts {
				close(out)
			}
		}(mx, destOuts)
	}
}

// startRelayBroadcast runs the relay mixer and broadcasts its output to all guest guilds.
// Calls ownerCleanup only after the mixer has fully stopped and its output channel is closed,
// ensuring no in-flight frames are lost and cleanup is ordered after the last broadcast.
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func(), guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	go func() {
		defer func() {
			ownerCleanup()
			sm.SessionStopped(ctx, guildID)
			trace.SpanFromContext(ctx).End()
			slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", guildID.String()))
		}()
		go relayMixer.Run(ctx)
		// Range blocks until the mixer closes its output channel (on ctx cancel),
		// guaranteeing all queued frames are broadcast before cleanup runs.
		for pkt := range relayMixer.Output() {
			relaySession.BroadcastFromGuild(guildID, pkt)
		}
	}()
}

// startDirectSessionCleanup waits for ctx cancellation then runs teardown.
func startDirectSessionCleanup(ctx context.Context, ownerCleanup func(), guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	go func() {
		defer func() {
			ownerCleanup()
			sm.SessionStopped(ctx, guildID)
			trace.SpanFromContext(ctx).End()
			slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", guildID.String()))
		}()
		<-ctx.Done()
	}()
}

// startGuestRelayBroadcast runs the guest relay mixer and broadcasts its output
// to all OTHER guilds via BroadcastFromGuild, excluding the guest itself.
func startGuestRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, session *ally.Session, guestGuildID snowflake.ID) {
	go relayMixer.Run(ctx)
	go func() {
		for pkt := range relayMixer.Output() {
			session.BroadcastFromGuild(guestGuildID, pkt)
		}
	}()
}

// registerRelayInputs wires a guild as a relay receiver in the ally session.
// A single Opus input channel is registered with the session. One bridge
// goroutine decodes each incoming packet exactly once and fans the resulting
// opus.Frame out to every destination channel mixer — mirroring what
// runFanoutSource does for local sources. Returns the single Opus input channel
// so the caller can close it on teardown (closing triggers bridge goroutine exit,
// which then closes all downstream frame channels).
func registerRelayInputs(ctx context.Context, guildID snowflake.ID, session *ally.Session, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, sm *telemetry.SessionMetrics) []chan<- []byte {
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
	// Pre-compute drop option once to avoid per-frame allocations on the hot path.
	relayDropOpt := sm.DropOption(guildID, "relay_bridge")
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
			// Drain to latest relay packet only when a backlog has built up,
			// avoiding drops under normal jitter where two packets arrive close together.
			if len(in) > relayBridgeDrainThreshold {
			drainRelay:
				for {
					select {
					case newer, ok := <-in:
						if !ok {
							return
						}
						pkt = newer
					default:
						break drainRelay
					}
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
				select {
				case out <- opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: now}:
				default:
					opus.PutPCM(pcm) // channel full — drop frame
					sm.FrameDropped(ctx, relayDropOpt)
				}
			}
		}
	}(ctx, relayOpusIn, frameOuts)

	session.AddGuild(guildID, []chan<- []byte{relayOpusIn})
	return []chan<- []byte{relayOpusIn}
}
