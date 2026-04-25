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
func endSession(ctx context.Context, ownerCleanup func(), guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	ownerCleanup()
	sm.SessionStopped(ctx, guildID)
	trace.SpanFromContext(ctx).End()
	slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", guildID.String()))
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
	drop := sm.FrameDropper(ctx, guildID, telemetry.DropPathMixer)
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
					drop()
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

		tryAddMixerInput(ctx, relayMixer, src.id, "relay mixer", &fanTargets, &removals)

		for _, dest := range dests {
			if dest.channelID == src.channelID {
				continue // mix-minus: don't relay audio back to its origin channel
			}
			tryAddMixerInput(ctx, chanMixers[dest.channelID], src.id, "channel mixer", &fanTargets, &removals)
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

		go runFanoutSource(ctx, guildID, src.ch, fanTargets, removals, sm)
	}
}

// runFanoutOwnerStar handles the owner source in host star topology.
// Raw Opus bytes are sent directly to directOuts (speaker chOuts — no decode needed,
// they have exactly one source). Simultaneously, the packet is decoded once and the
// resulting Frame is forwarded to relayMixCh so the relay mixer can broadcast to guests.
// Closes directOuts when the source closes or ctx is cancelled.
func runFanoutOwnerStar(ctx context.Context, guildID snowflake.ID, in <-chan []byte, directOuts []chan<- []byte, relayMixCh chan opus.Frame, relayMixer *opus.Mixer, srcID snowflake.ID, sm *telemetry.SessionMetrics) {
	dropDirect := sm.FrameDropper(ctx, guildID, telemetry.DropPathOwnerStarDirect)
	dropRelay := sm.FrameDropper(ctx, guildID, telemetry.DropPathOwnerStarRelay)
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
					dropDirect()
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
				dropRelay()
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
			go runFanoutOwnerStar(ctx, guildID, src.ch, directSpeakerOuts, relayCh, relayMixer, src.id, sm)
		} else {
			// Speaker source: decode once → hub mixer only (star spoke → hub).
			// Speaker audio is NOT relayed to guests — only the owner/caller's audio is.
			var fanTargets []chan opus.Frame
			var removals []mixerRef

			if hubMixer, ok := chanMixers[ownerChannelID]; ok {
				tryAddMixerInput(ctx, hubMixer, src.id, "hub mixer", &fanTargets, &removals)
			}

			go runFanoutSource(ctx, guildID, src.ch, fanTargets, removals, sm)
		}
	}
}

// startFanoutDirect is the bypass path for RaidModeOneCaller.
// Raw Opus packets are read from in and copied to every speaker output channel
// and the relay session without any PCM decode/encode step.
// The goroutine closes all outs when it exits so VoiceProviders shut down cleanly.
func startFanoutDirect(ctx context.Context, in <-chan []byte, outs []chan<- []byte, session *ally.Session, guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	drop := sm.FrameDropper(ctx, guildID, telemetry.DropPathDirect)
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
						drop()
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
func startChannelMixers(ctx context.Context, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	for _, dest := range dests {
		mx := chanMixers[dest.channelID]
		destOuts := dest.outs
		drop := sm.FrameDropper(ctx, guildID, telemetry.DropPathChannelMixer)
		go mx.Run(ctx)
		go func(mx *opus.Mixer, destOuts []chan<- []byte, drop func()) {
			for pkt := range mx.Output() {
				for _, out := range destOuts {
					select {
					case out <- pkt:
					default:
						drop()
					}
				}
			}
			for _, out := range destOuts {
				close(out)
			}
		}(mx, destOuts, drop)
	}
}

// startRelayBroadcast runs the relay mixer and broadcasts its output to all guest guilds.
// Calls endSession only after the mixer has fully stopped and its output channel is closed,
// ensuring no in-flight frames are lost and cleanup is ordered after the last broadcast.
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func(), guildID snowflake.ID, sm *telemetry.SessionMetrics) {
	go func() {
		defer endSession(ctx, ownerCleanup, guildID, sm)
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
		defer endSession(ctx, ownerCleanup, guildID, sm)
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
	drop := sm.FrameDropper(ctx, guildID, telemetry.DropPathRelayBridge)
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
					drop()
				}
			}
		}
	}(ctx, relayOpusIn, frameOuts)

	session.AddGuild(guildID, []chan<- []byte{relayOpusIn})
	return []chan<- []byte{relayOpusIn}
}
