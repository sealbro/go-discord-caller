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

// mixerRef pairs a mixer with the input ID registered in it. Used by the
// router-driven install closures to detach the mixer inputs they allocated
// when transitioning out of mix mode or closing the session.
type mixerRef struct {
	mx *opus.Mixer
	id snowflake.ID
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
func registerRelayInputs(_ context.Context, gm telemetry.GuildMetrics, session *ally.Session, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) []chan<- []byte {
	type relaySource struct {
		src *opus.SourceBuffer
		mx  *opus.Mixer
	}

	drop := gm.Drop(telemetry.DropPathRelayBridge)
	relaySources := make([]relaySource, 0, len(dests))
	for _, dest := range dests {
		mx := chanMixers[dest.channelID]
		src := opus.NewSourceBuffer(drop)
		if err := mx.AddInput(relayInputID, src); err != nil {
			slog.Warn("relay: failed to add input to channel mixer",
				slog.String("channelID", dest.channelID.String()),
				slog.Any("err", err))
			continue
		}
		relaySources = append(relaySources, relaySource{src: src, mx: mx})
	}
	if len(relaySources) == 0 {
		return nil
	}

	// Single Opus input channel shared across all destination mixers.
	// Bridge: decode once, fan Frame into every SourceBuffer via Feed.
	// Feed handles overflow internally (drops oldest, recycles pool buffers).
	// Exits when relayOpusIn is closed; deferred cleanup detaches mixer inputs.
	relayOpusIn := make(chan []byte, audioChanBuf)
	go func() {
		defer func() {
			for _, rs := range relaySources {
				rs.mx.RemoveInput(relayInputID)
				rs.src.Drain()
			}
		}()
		dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
		if err != nil {
			slog.Error("relay bridge: failed to create decoder", slog.Any("err", err))
			return
		}
		scratch := make([]int16, opus.MixerPCMBuf)
		for pkt := range relayOpusIn {
			if len(pkt) == 0 {
				continue
			}
			n, err := dec.Decode(pkt, scratch)
			if err != nil {
				slog.Debug("relay bridge: decode failed", slog.Any("err", err))
				continue
			}
			now := time.Now()
			for _, rs := range relaySources {
				pcm := opus.GetPCM()[:n*opus.MixerChannels]
				copy(pcm, scratch[:n*opus.MixerChannels])
				opusCopy := opus.CopyOpusFrame(pkt)
				rs.src.Feed(opus.Frame{PCM: pcm, Opus: opusCopy, CreatedAt: now})
			}
		}
	}()

	session.AddGuild(gm.GuildID(), []chan<- []byte{relayOpusIn})
	return []chan<- []byte{relayOpusIn}
}
