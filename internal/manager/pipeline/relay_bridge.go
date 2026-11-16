package pipeline

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// relayInBuf is the bridge's inbound queue. It is drained as fast as packets
// can be decoded rather than at the frame rate, so its depth adds no latency;
// it only has to hold what several peers deliver in the same instant.
const relayInBuf = 4 * opus.AudioChanBuf

// relayPeerReapInterval is how often the bridge releases the inputs of guilds
// that have left the session.
const relayPeerReapInterval = 5 * time.Second

type relayPacket struct {
	src  snowflake.ID
	opus []byte
}

// relayPeer is one peer guild's stream as the destination mixers see it.
type relayPeer struct {
	inputID snowflake.ID
	dec     *hraban.Decoder
	buffers []*opus.SourceBuffer // one per destination mixer, in mixer order
}

// relayBridge decodes what the other guilds of an ally session send this one
// and feeds it to the destination mixers — the relay equivalent of what
// installFanoutSource sets up for local VoiceReceiver-backed sources. It is a
// goroutine because relay packets arrive from another guild's goroutine rather
// than via inline ReceiveOpusFrame. Each packet is decoded exactly once,
// however many mixers it reaches.
//
// Every peer guild gets a decoder and a mixer input of its own. Each one
// broadcasts an independent Opus stream, so two of them talking through one
// decoder corrupt its state for both, and through one input they arrive at
// twice the rate the mixer empties it. Kept apart they are simply two inputs,
// which the mixer sums like any two local speakers.
//
// The first peer heard takes the input registered up front under RelayInputID,
// so a session of two guilds — one peer — never allocates on the audio path.
type relayBridge struct {
	in         chan relayPacket
	session    *ally.Session
	mixers     []*opus.Mixer
	drop       func()
	newDecoder func(sampleRate, channels int) (*hraban.Decoder, error)

	// The rest is touched only by the bridge goroutine.
	primary     *relayPeer
	primaryFree bool
	// peers is keyed by source guild. A nil entry is a peer whose decoder could
	// not be created; its packets are dropped until it leaves.
	peers   map[snowflake.ID]*relayPeer
	scratch []int16
}

// registerRelayInputs wires a guild as a relay receiver in the ally session,
// feeding every destination's mixer. It returns nil when there is nothing to
// feed or the decoder cannot be created; otherwise the caller must detach the
// guild from the session and then stop the bridge on teardown.
func registerRelayInputs(gm telemetry.GuildMetrics, session *ally.Session, dests []*DestChannel, chanMixers map[snowflake.ID]*opus.Mixer, newDecoder func(sampleRate, channels int) (*hraban.Decoder, error)) *relayBridge {
	b := newRelayBridge(gm, session, dests, chanMixers, newDecoder)
	if b == nil {
		return nil
	}
	go b.run()
	session.AddReceiver(gm.GuildID(), b.receive)
	return b
}

func newRelayBridge(gm telemetry.GuildMetrics, session *ally.Session, dests []*DestChannel, chanMixers map[snowflake.ID]*opus.Mixer, newDecoder func(sampleRate, channels int) (*hraban.Decoder, error)) *relayBridge {
	if len(dests) == 0 {
		return nil
	}
	dec, err := newDecoder(opus.MixerSampleRate, opus.MixerChannels)
	if err != nil {
		slog.Error("relay bridge: failed to create decoder", slog.Any("err", err))
		return nil
	}

	b := &relayBridge{
		in:          make(chan relayPacket, relayInBuf),
		session:     session,
		drop:        gm.Drop(telemetry.DropPathRelayBridge),
		newDecoder:  newDecoder,
		primaryFree: true,
		peers:       make(map[snowflake.ID]*relayPeer),
		scratch:     make([]int16, opus.MixerPCMBuf),
	}
	for _, dest := range dests {
		b.mixers = append(b.mixers, chanMixers[dest.ChannelID])
	}
	b.primary = b.attach(RelayInputID, dec)
	return b
}

// receive is the guild's ally.RelaySink.
func (b *relayBridge) receive(src snowflake.ID, pkt []byte) {
	select {
	case b.in <- relayPacket{src: src, opus: pkt}:
	default:
		opus.PutEncodedFrame(pkt)
		b.drop()
	}
}

// stop ends the bridge, which detaches its inputs from the mixers on the way
// out. The guild must already be removed from the session, so that no
// broadcast can still be sending to it (see ally.Session.broadcast).
func (b *relayBridge) stop() {
	close(b.in)
}

func (b *relayBridge) run() {
	defer b.detachAll()
	reap := time.NewTicker(relayPeerReapInterval)
	defer reap.Stop()
	for {
		select {
		case pkt, ok := <-b.in:
			if !ok {
				return
			}
			b.handle(pkt)
		case <-reap.C:
			b.reapDeparted()
		}
	}
}

// handle decodes one packet with its peer's decoder and feeds the frame to
// that peer's input on every destination mixer.
func (b *relayBridge) handle(pkt relayPacket) {
	defer opus.PutEncodedFrame(pkt.opus)
	if len(pkt.opus) == 0 {
		return
	}
	peer := b.peerFor(pkt.src)
	if peer == nil {
		return
	}
	n, err := peer.dec.Decode(pkt.opus, b.scratch)
	if err != nil {
		slog.Debug("relay bridge: decode failed", slog.Any("err", err))
		return
	}
	now := time.Now()
	for _, buf := range peer.buffers {
		pcm := opus.GetPCM()[:n*opus.MixerChannels]
		copy(pcm, b.scratch[:n*opus.MixerChannels])
		buf.Feed(opus.Frame{PCM: pcm, Opus: opus.CopyOpusFrame(pkt.opus), CreatedAt: now})
	}
}

func (b *relayBridge) peerFor(src snowflake.ID) *relayPeer {
	if peer, known := b.peers[src]; known {
		return peer
	}
	if b.primaryFree {
		b.primaryFree = false
		b.peers[src] = b.primary
		return b.primary
	}
	dec, err := b.newDecoder(opus.MixerSampleRate, opus.MixerChannels)
	if err != nil {
		slog.Error("relay bridge: failed to create a decoder for another guild; its audio is dropped",
			slog.Any("err", err))
		b.peers[src] = nil
		return nil
	}
	// A guild ID cannot collide with the mixers' other inputs: those are
	// RelayInputID and the router's synthetic IDs, which have bit 63 set.
	peer := b.attach(src, dec)
	b.peers[src] = peer
	return peer
}

func (b *relayBridge) attach(inputID snowflake.ID, dec *hraban.Decoder) *relayPeer {
	peer := &relayPeer{inputID: inputID, dec: dec}
	for _, mx := range b.mixers {
		buf := opus.NewSourceBuffer(b.drop)
		mx.AddInput(inputID, buf)
		peer.buffers = append(peer.buffers, buf)
	}
	return peer
}

func (b *relayBridge) detach(peer *relayPeer) {
	for i, mx := range b.mixers {
		mx.RemoveInput(peer.inputID)
		peer.buffers[i].Drain()
	}
}

// reapDeparted releases the inputs of peers no longer in the session. The
// up-front input stays registered and goes to the next new peer, with a fresh
// decoder where one can be had: the old one's state belongs to another stream.
func (b *relayBridge) reapDeparted() {
	for src, peer := range b.peers {
		if b.session.Attached(src) {
			continue
		}
		delete(b.peers, src)
		switch {
		case peer == nil:
		case peer == b.primary:
			for _, buf := range peer.buffers {
				buf.Drain()
			}
			if dec, err := b.newDecoder(opus.MixerSampleRate, opus.MixerChannels); err == nil {
				peer.dec = dec
			}
			b.primaryFree = true
		default:
			b.detach(peer)
		}
	}
}

func (b *relayBridge) detachAll() {
	b.detach(b.primary)
	for _, peer := range b.peers {
		if peer != nil && peer != b.primary {
			b.detach(peer)
		}
	}
}

// hostRelayInputs owns the relay bridge a host pipeline registers, which runs
// until it is stopped. A /stop can tear the session down before start
// registers it, so registering after close is a no-op rather than a leak.
type hostRelayInputs struct {
	mu     sync.Mutex
	bridge *relayBridge
	closed bool
}

func (r *hostRelayInputs) register(registerFn func() *relayBridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.bridge = registerFn()
	}
}

// close detaches guildID from the session before stopping its bridge, so no
// broadcast can send on a closed channel (see ally.Session.broadcast).
func (r *hostRelayInputs) close(session *ally.Session, guildID snowflake.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.bridge == nil {
		return
	}
	session.RemoveGuild(guildID)
	r.bridge.stop()
}
