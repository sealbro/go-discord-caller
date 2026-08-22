package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

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
// scheduler wake-up per produced frame.
//
// Historical note (teardown-close-race): this goroutine used to close destOuts
// after Run returned, on the assumption it was their sole writer. That is
// false in copy mode (router.RouteCopy), where opus.VoiceReceiver.dispatchFanout
// writes the raw Opus packet straight into these same channels on the UDP
// receive goroutine — so close(out) raced that send (caught by `go test -race`
// in the integration suite, and a send-on-closed panic hazard). With two
// producers, neither may own the close, so the channels are left for GC:
// VoiceProvider exits on its own Close() (v.done), which teardown invokes via
// BuildSpeakerCleanup, so no goroutine leaks. The single-teardown-step half of
// the fix — stopping every writer before reclaiming the channels — now lives
// in router.Router.Close(), which re-Installs an empty FanoutInstall{} on
// every source (silencing dispatchFanout for both copy and mix mode) before
// session.Cleanup calls speakerCleanup(). Re-validate with the integration
// -race suite when revisiting either side of this.
// relayFeed, when non-nil, is the destination's relay-feed predicate; it stops
// the DrainWatcher from auto-pausing a mixer that a peer guild may feed at any
// moment (see DrainWatcher.WithKeepAlive). Pass nil for mixers no relay input
// reaches.
func StartChannelMixers(ctx context.Context, gm telemetry.GuildMetrics, dests []*DestChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayFeed func() bool) {
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
		go mx.Run(ctx)
		go opus.NewDrainWatcher(mx, opus.DrainIdleTimeout).WithKeepAlive(relayFeed).Run(ctx)
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
	relayOpusIn := make(chan []byte, opus.AudioChanBuf)
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

// RelayFeedFor returns the router.DestSlot.RelayFeed predicate for guildID:
// true while some OTHER guild in the ally session may broadcast audio in.
// Attach it to every destination that RegisterRelayInputs feeds, so the router
// keeps that destination's mixer running even when the local guild is silent.
func RelayFeedFor(session *ally.Session, guildID snowflake.ID) func() bool {
	return func() bool { return session.HasCapturingPeers(guildID) }
}

// WatchRelayMembership makes r.Recompute run whenever the session's set of
// attached or capturing guilds changes, so relay-fed destinations unpause as
// soon as a peer guild joins (and fall back to copy mode when it leaves).
// When capturing is true, guildID is also registered as a guild that may
// broadcast into the session, which is what flips its peers' RelayFeed.
func WatchRelayMembership(session *ally.Session, guildID snowflake.ID, r *router.Router, capturing bool) {
	session.SetRouteObserver(guildID, r.Recompute)
	if capturing {
		session.SetCapturing(guildID)
	}
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
