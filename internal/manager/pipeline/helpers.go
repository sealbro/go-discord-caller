package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// audioChanBuf is the buffer size for the single-producer/single-consumer
// Opus channels constructed inside the pipeline (relayOpusIn). Three frames ×
// 20 ms = 60 ms of jitter tolerance before the bleed-off path engages.
const audioChanBuf = 3

// EndSession runs the common host session teardown: invokes ownerCleanup, records
// the stop metric, ends the tracing span, and logs the session end.
// Intended to be called as a deferred statement inside session goroutines.
func EndSession(ctx context.Context, ownerCleanup func(), gm telemetry.GuildMetrics) {
	ownerCleanup()
	gm.SessionStopped()
	trace.SpanFromContext(ctx).End()
	slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", gm.GuildID().String()))
}

// StartChannelMixers runs each per-channel mixer with a sink that distributes
// produced frames directly to every speaker output channel for the destination.
// Removing the per-mixer forwarder goroutine cuts one channel hop and one
// scheduler wake-up per produced frame. destOuts are closed after Run returns
// so VoiceProvider goroutines shut down cleanly; this goroutine is the sole
// writer to destOuts after the sink stops being invoked (Run has returned).
func StartChannelMixers(ctx context.Context, gm telemetry.GuildMetrics, dests []*DestChannel, chanMixers map[snowflake.ID]*opus.Mixer) {
	drop := gm.Drop(telemetry.DropPathChannelMixer)
	for _, dest := range dests {
		mx := chanMixers[dest.ChannelID]
		destOuts := dest.Outs
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

// StartRelayBroadcast runs the relay mixer with a sink that broadcasts each
// produced frame directly to all guest guilds. EndSession runs after Run
// returns, by which time tick (and therefore the sink) is no longer invoked,
// so cleanup is ordered after the last broadcast.
func StartRelayBroadcast(ctx context.Context, gm telemetry.GuildMetrics, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func()) {
	guildID := gm.GuildID()
	relayMixer.SetSink(func(pkt []byte) {
		relaySession.BroadcastFromGuild(guildID, pkt)
	})
	go func() {
		defer EndSession(ctx, ownerCleanup, gm)
		relayMixer.Run(ctx)
	}()
}

// StartGuestRelayBroadcast runs the guest relay mixer with a sink that
// broadcasts each produced frame directly to all OTHER guilds via
// BroadcastFromGuild, excluding the guest itself.
func StartGuestRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, session *ally.Session, guestGuildID snowflake.ID) {
	relayMixer.SetSink(func(pkt []byte) {
		session.BroadcastFromGuild(guestGuildID, pkt)
	})
	go relayMixer.Run(ctx)
}

// RegisterRelayInputs wires a guild as a relay receiver in the ally session.
// A single Opus input channel is registered with the session. One bridge
// goroutine decodes each incoming packet exactly once and fans the resulting
// opus.Frame out to every destination channel mixer — the relay equivalent of
// what installFanoutSource sets up for local VoiceReceiver-backed sources
// (the bridge stays as a goroutine because relay packets arrive on a chan
// from another guild rather than via inline ReceiveOpusFrame). Returns the
// single Opus input channel so the caller can close it on teardown (closing
// triggers bridge goroutine exit, which then closes all downstream frame
// channels).
func RegisterRelayInputs(_ context.Context, gm telemetry.GuildMetrics, session *ally.Session, dests []*DestChannel, chanMixers map[snowflake.ID]*opus.Mixer) []chan<- []byte {
	type relaySource struct {
		src *opus.SourceBuffer
		mx  *opus.Mixer
	}

	drop := gm.Drop(telemetry.DropPathRelayBridge)
	relaySources := make([]relaySource, 0, len(dests))
	for _, dest := range dests {
		mx := chanMixers[dest.ChannelID]
		src := opus.NewSourceBuffer(drop)
		if err := mx.AddInput(RelayInputID, src); err != nil {
			slog.Warn("relay: failed to add input to channel mixer",
				slog.String("channelID", dest.ChannelID.String()),
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
	relayOpusIn := make(chan []byte, audioChanBuf)
	go func() {
		defer func() {
			for _, rs := range relaySources {
				rs.mx.RemoveInput(RelayInputID)
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

// IterDeduplicatedCaptures calls fn for the first capturing speaker per voice
// channel across joined. Subsequent speakers in the same channel keep their
// FanoutHandle uninstalled — frames received by their VoiceReceiver are silently
// dropped in dispatchFanout (state == nil), so no drain goroutine is needed.
func IterDeduplicatedCaptures(joined []SpeakerResult, fn func(SpeakerResult)) {
	seen := map[snowflake.ID]bool{}
	for _, r := range joined {
		if r.Handle == nil {
			continue
		}
		if seen[r.GV.ChannelID()] {
			continue
		}
		seen[r.GV.ChannelID()] = true
		fn(r)
	}
}

// BuildSources returns a deduplicated list of audio sources for host pipelines:
// the owner bot plus one capturing speaker per voice channel. When two speaker
// bots share a channel only the first is wired into the mixer graph.
func BuildSources(ownerUserID, ownerChannelID snowflake.ID, ownerHandle *opus.FanoutHandle, joined []SpeakerResult) []SourceEntry {
	sources := []SourceEntry{{ownerUserID, ownerChannelID, ownerHandle}}
	IterDeduplicatedCaptures(joined, func(r SpeakerResult) {
		sources = append(sources, SourceEntry{r.Speaker.ID, r.GV.ChannelID(), r.Handle})
	})
	return sources
}

// BuildGuestSources returns deduplicated capture sources from speaker joins.
// Unlike the host's BuildSources, the guest owner bot is provider-only so
// it contributes no capture handle.
func BuildGuestSources(joined []SpeakerResult) []SourceEntry {
	var sources []SourceEntry
	IterDeduplicatedCaptures(joined, func(r SpeakerResult) {
		sources = append(sources, SourceEntry{r.Speaker.ID, r.GV.ChannelID(), r.Handle})
	})
	return sources
}

// BuildDestinations groups each speaker's output channel by its destination voice channel.
func BuildDestinations(joined []SpeakerResult) []*DestChannel {
	destMap := map[snowflake.ID]*DestChannel{}
	for _, r := range joined {
		d, ok := destMap[r.GV.ChannelID()]
		if !ok {
			d = &DestChannel{ChannelID: r.GV.ChannelID()}
			destMap[r.GV.ChannelID()] = d
		}
		d.Outs = append(d.Outs, r.ChOut)
	}
	dests := make([]*DestChannel, 0, len(destMap))
	for _, d := range destMap {
		dests = append(dests, d)
	}
	return dests
}

// BuildSpeakerCleanup returns a function that closes every speaker's
// provider/receiver and leaves its voice channel, exactly once.
// Leave calls run in parallel so teardown is bounded by the slowest connection
// rather than N×VoiceLeaveTimeout.
func BuildSpeakerCleanup(guildID snowflake.ID, joined []SpeakerResult) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), VoiceLeaveTimeout)
			defer cancel()
			var wg sync.WaitGroup
			wg.Add(len(joined))
			for _, r := range joined {
				go func(r SpeakerResult) {
					defer wg.Done()
					if r.Cleanup != nil {
						r.Cleanup()
					}
					r.GV.Leave(ctx, guildID)
				}(r)
			}
			wg.Wait()
		})
	}
}
