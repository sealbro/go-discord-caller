package pipeline

import (
	"bytes"
	"context"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	relayTestReceiver = snowflake.ID(50)
	relayTestPeerA    = snowflake.ID(60)
	relayTestPeerB    = snowflake.ID(70)
	relayTestChannel  = snowflake.ID(2001)
)

// tonePackets encodes n 20 ms frames of a tone the way a peer's relay mixer
// would, each stream with an encoder of its own.
func tonePackets(t testing.TB, freq float64, n int) [][]byte {
	t.Helper()
	enc, err := hraban.NewEncoder(opus.MixerSampleRate, opus.MixerChannels, hraban.AppAudio)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	if err := enc.SetBitrate(64000); err != nil {
		t.Fatalf("set bitrate: %v", err)
	}
	raw := make([]byte, 4096)
	packets := make([][]byte, n)
	for i := range packets {
		size, err := enc.Encode(tonePCM(freq, i), raw)
		if err != nil {
			t.Fatalf("encode frame %d: %v", i, err)
		}
		packets[i] = append([]byte(nil), raw[:size]...)
	}
	return packets
}

func relayBridgeDrops(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "gdc.fanout.frames.dropped.total" {
				continue
			}
			for _, dp := range sum.DataPoints {
				if path, _ := dp.Attributes.Value("path"); path.AsString() == string(telemetry.DropPathRelayBridge) {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// Every capturing guild of a session broadcasts a stream of its own, and a
// session of three has two of them reaching each guild. Sharing one mixer
// input between them halves the audio as soon as both talk: 100 frames a
// second go into a buffer the mixer empties at 50.
func TestRelayInputKeepsTwoTalkingPeersApart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("relay_peers_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, relayTestReceiver)
	mx, err := opus.NewMixer(gm.Opus)
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	var emitted atomic.Int64
	mx.SetSink(func(pkt []byte) {
		emitted.Add(1)
		opus.PutEncodedFrame(pkt)
	})

	session := ally.NewManager().Create("TESTCODE", relayTestReceiver, guild.RaidModeGuildCaller)
	if got := registerRelayInputs(gm, session, []*DestChannel{{ChannelID: relayTestChannel}},
		map[snowflake.ID]*opus.Mixer{relayTestChannel: mx}, hraban.NewDecoder); got == nil {
		t.Fatal("relay input was not registered")
	}
	go mx.Run(ctx)

	const frames = 50
	peerA, peerB := tonePackets(t, 140, frames), tonePackets(t, 230, frames)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := range frames {
		<-tick.C
		session.BroadcastFromGuild(relayTestPeerA, opus.CopyOpusFrame(peerA[i]))
		session.BroadcastFromGuild(relayTestPeerB, opus.CopyOpusFrame(peerB[i]))
	}
	time.Sleep(100 * time.Millisecond)
	cancel()

	if lost := relayBridgeDrops(t, reader); lost > frames/10 {
		t.Errorf("%d of %d frames from two talking peers were evicted from the relay input", lost, 2*frames)
	}
	if got := emitted.Load(); got < frames*8/10 {
		t.Errorf("mixer emitted %d frames for %d ticks of two peers talking", got, frames)
	}
}

// tonePCM is one stereo 20 ms frame of a harmonic tone, continuous across
// consecutive frames.
func tonePCM(freq float64, frameIdx int) []int16 {
	const samples = opus.MixerPCMBuf / opus.MixerChannels
	pcm := make([]int16, opus.MixerPCMBuf)
	for i := range samples {
		ts := float64(frameIdx*samples+i) / float64(opus.MixerSampleRate)
		v := 0.0
		for harmonic := 1; harmonic <= 5; harmonic++ {
			v += math.Sin(2*math.Pi*freq*float64(harmonic)*ts) / float64(harmonic)
		}
		pcm[i*2], pcm[i*2+1] = int16(6000*v), int16(6000*v)
	}
	return pcm
}

func newTestBridge(t testing.TB, destinations int) (*relayBridge, *ally.Session, []*opus.Mixer) {
	t.Helper()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider().Meter("relay_peers_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(context.Background(), relayTestReceiver)
	session := ally.NewManager().Create("TESTCODE", relayTestReceiver, guild.RaidModeGuildCaller)
	session.AddListener(relayTestPeerA, nil)
	session.AddListener(relayTestPeerB, nil)

	var dests []*DestChannel
	var mixers []*opus.Mixer
	byChannel := map[snowflake.ID]*opus.Mixer{}
	for i := range destinations {
		mx, err := opus.NewMixer(gm.Opus)
		if err != nil {
			t.Fatalf("NewMixer: %v", err)
		}
		channelID := relayTestChannel + snowflake.ID(i)
		dests = append(dests, &DestChannel{ChannelID: channelID})
		byChannel[channelID] = mx
		mixers = append(mixers, mx)
	}
	b := newRelayBridge(gm, session, dests, byChannel, hraban.NewDecoder)
	if b == nil {
		t.Fatal("no relay bridge")
	}
	return b, session, mixers
}

func decodeAll(t *testing.T, packets [][]byte) [][]int16 {
	t.Helper()
	dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	frames := make([][]int16, len(packets))
	for i, pkt := range packets {
		pcm := make([]int16, opus.MixerPCMBuf)
		n, err := dec.Decode(pkt, pcm)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		frames[i] = pcm[:n*opus.MixerChannels]
	}
	return frames
}

// An Opus decoder carries state from one packet of a stream to the next. Fed
// two peers' packets in turn it predicts each from the other, and both come
// out distorted; each peer must decode exactly as it would alone.
func TestRelayBridgeDecodesEachPeerWithItsOwnDecoder(t *testing.T) {
	b, _, _ := newTestBridge(t, 1)
	const frames = 40
	streams := map[snowflake.ID][][]byte{
		relayTestPeerA: tonePackets(t, 140, frames),
		relayTestPeerB: tonePackets(t, 230, frames),
	}
	want := map[snowflake.ID][][]int16{
		relayTestPeerA: decodeAll(t, streams[relayTestPeerA]),
		relayTestPeerB: decodeAll(t, streams[relayTestPeerB]),
	}

	for i := range frames {
		for _, src := range []snowflake.ID{relayTestPeerA, relayTestPeerB} {
			b.handle(relayPacket{src: src, opus: opus.CopyOpusFrame(streams[src][i])})
			got, ok := b.peers[src].buffers[0].Pull()
			if !ok {
				t.Fatalf("frame %d of peer %d never reached its mixer input", i, src)
			}
			if !slices.Equal(got.PCM, want[src][i]) {
				t.Fatalf("frame %d of peer %d decoded differently from a decoder of its own", i, src)
			}
			if !bytes.Equal(got.Opus, streams[src][i]) {
				t.Fatalf("frame %d of peer %d carries another packet's Opus bytes", i, src)
			}
			opus.PutPCM(got.PCM)
			opus.PutEncodedFrame(got.Opus)
		}
	}
}

func TestRelayBridgeGivesEachPeerItsOwnMixerInput(t *testing.T) {
	b, session, mixers := newTestBridge(t, 2)
	packet := tonePackets(t, 140, 1)[0]

	b.handle(relayPacket{src: relayTestPeerA, opus: opus.CopyOpusFrame(packet)})
	for _, mx := range mixers {
		if got := mx.InputIDs(); !slices.Equal(got, []snowflake.ID{RelayInputID}) {
			t.Fatalf("one peer: mixer inputs = %v, want only the relay input registered up front", got)
		}
	}

	b.handle(relayPacket{src: relayTestPeerB, opus: opus.CopyOpusFrame(packet)})
	for _, mx := range mixers {
		if got := len(mx.InputIDs()); got != 2 {
			t.Fatalf("two peers: mixer has %d inputs, want one each", got)
		}
	}

	session.RemoveGuild(relayTestPeerB)
	b.reapDeparted()
	for _, mx := range mixers {
		if got := mx.InputIDs(); !slices.Equal(got, []snowflake.ID{RelayInputID}) {
			t.Errorf("after a peer left: mixer inputs = %v, want its input released", got)
		}
	}

	// The peer holding the up-front input leaves: the input stays, and the
	// next peer to speak takes it over.
	session.RemoveGuild(relayTestPeerA)
	b.reapDeparted()
	b.handle(relayPacket{src: relayTestPeerB, opus: opus.CopyOpusFrame(packet)})
	for _, mx := range mixers {
		if got := mx.InputIDs(); !slices.Equal(got, []snowflake.ID{RelayInputID}) {
			t.Errorf("after every peer left and one returned: mixer inputs = %v, want only the relay input", got)
		}
	}
}

// One peer is every two-guild session, the common case: the per-peer lookup
// must not put an allocation on its audio path.
func TestRelayBridgeHandlesAKnownPeerWithoutAllocating(t *testing.T) {
	if raceDetector {
		t.Skip("sync.Pool drops buffers at random under the race detector")
	}
	b, _, _ := newTestBridge(t, 3)
	packet := tonePackets(t, 140, 1)[0]
	handle := func() { b.handle(relayPacket{src: relayTestPeerA, opus: opus.CopyOpusFrame(packet)}) }
	handle()
	if allocs := testing.AllocsPerRun(500, handle); allocs != 0 {
		t.Errorf("handling a packet allocates %.0f times, want 0", allocs)
	}
}

func BenchmarkRelayBridgeHandle(b *testing.B) {
	for _, peers := range []int{1, 2, 4} {
		b.Run("peers="+string(rune('0'+peers)), func(b *testing.B) {
			bridge, _, _ := newTestBridge(b, 3)
			streams := make([][][]byte, peers)
			for i := range streams {
				streams[i] = tonePackets(b, 140+90*float64(i), 50)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				peer := i % peers
				bridge.handle(relayPacket{
					src:  relayTestPeerA + snowflake.ID(peer),
					opus: opus.CopyOpusFrame(streams[peer][(i/peers)%50]),
				})
			}
		})
	}
}

// listenerOuts swaps the fixture's speaker queues for one the test can read.
func listenerOuts(p *GuestParams) <-chan []byte {
	out := make(chan []byte, opus.AudioChanBuf)
	p.Setup.Joined = nil
	p.Setup.Outs = []chan<- []byte{out}
	return out
}

func countPackets(ctx context.Context, out <-chan []byte) *atomic.Int64 {
	var n atomic.Int64
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case pkt := <-out:
				n.Add(1)
				opus.PutEncodedFrame(pkt)
			}
		}
	}()
	return &n
}

// A listener guest of a host that shares its mix with capturing guests is sent
// one stream by the host and one by each of those guests. Its speaker bots can
// play a single stream, so the streams have to reach them mixed into one.
func TestListenerGuestMixesThePeersOfASharedMix(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeGuildCaller, guild.RaidModeAllyListener)
	out := listenerOuts(&p)
	const otherGuest = snowflake.ID(70)
	p.AllySession.AddListener(otherGuest, nil)

	_, start, cleanup, err := GuestListenerPipeline{}.Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	start()
	t.Cleanup(func() {
		p.AllySession.RemoveGuild(fx.guestGuildID)
		cleanup()
	})
	played := countPackets(ctx, out)

	const frames = 50
	host, guest := tonePackets(t, 140, frames), tonePackets(t, 230, 2*frames)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for i := range frames {
		<-tick.C
		p.AllySession.BroadcastFromGuild(otherGuest, opus.CopyOpusFrame(guest[i]))
	}
	time.Sleep(100 * time.Millisecond)
	guestOnly := played.Load()
	if guestOnly < frames*8/10 {
		t.Errorf("speaker was handed %d packets while only another guest talked for %d ticks: the listener does not hear it",
			guestOnly, frames)
	}

	for i := range frames {
		<-tick.C
		p.AllySession.BroadcastFromGuild(fx.hostGuildID, opus.CopyOpusFrame(host[i]))
		p.AllySession.BroadcastFromGuild(otherGuest, opus.CopyOpusFrame(guest[frames+i]))
	}
	time.Sleep(100 * time.Millisecond)

	if got := played.Load() - guestOnly; got < frames*8/10 || got > frames*12/10 {
		t.Errorf("speaker was handed %d packets for %d ticks of two peers talking, want one mixed stream of about %d",
			got, frames, frames)
	}
}

// In the star topology guests reach the host's hub and hear only the host.
func TestListenerGuestOfAStarHostHearsOnlyTheHost(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fx := guestFx()
	p := buildGuestParams(t, ctx, fx, guild.RaidModeOneManyGuildCaller, guild.RaidModeAllyListener)
	out := listenerOuts(&p)
	const otherGuest = snowflake.ID(70)

	_, start, cleanup, err := GuestListenerPipeline{}.Build(ctx, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	start()
	t.Cleanup(func() {
		p.AllySession.RemoveGuild(fx.guestGuildID)
		cleanup()
	})

	p.AllySession.BroadcastFromGuild(otherGuest, []byte{2})
	p.AllySession.BroadcastFromGuild(fx.hostGuildID, []byte{1})

	if len(out) != 1 {
		t.Fatalf("speaker queue holds %d packets, want only the host's", len(out))
	}
	if got := <-out; got[0] != 1 {
		t.Errorf("speaker was handed packet %v, want the host's", got)
	}
}
