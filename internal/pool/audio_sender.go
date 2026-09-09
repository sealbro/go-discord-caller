package pool

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
)

// AudioSenderRegistry tracks the live audio sender behind each voice.Conn so
// teardown can stop it.
//
// disgo v0.19.3 never closes the audio sender when a connection is closed:
//
//	func (c *connImpl) Close(ctx context.Context) {
//		_ = c.voiceStateUpdateFunc(ctx, c.state.GuildID, nil, false, false)
//		defer c.gateway.Close()
//		defer func() { _ = c.udp.Close() }()
//		...
//		c.removeConnFunc()
//	}
//
// The sender goroutine survives, and its loop does not exit on a provider
// error either — defaultAudioSender.send logs and returns, and open() keeps
// ticking every 20 ms:
//
//	opus, err := s.opusProvider.ProvideOpusFrame()
//	if err != nil && err != io.EOF {
//		s.logger.Error("error while reading opus frame", slog.Any("err", err))
//		return // returns from send(), not from the loop
//	}
//
// Our teardown closes the provider (manager.VoiceConnSetup.Apply's cleanup),
// after which ProvideOpusFrame returns an error immediately rather than
// blocking. The two behaviours together turn a torn-down connection into an
// endless 50 lines/second ERROR loop.
//
// disgo does close the sender from connImpl.HandleVoiceStateUpdate when
// Discord reports ChannelID == nil, which is why most disconnects are quiet.
// The leak surfaces when that event never arrives — a degraded connection, a
// gateway that dies mid-teardown — and then it never stops on its own. On
// 2026-09-07 one speaker bot logged ~510k such lines over 2h50m and drove the
// day's largest CPU peak on the host.
//
// The registry is keyed by the voice.Conn that disgo passes to the create
// func, which is the same Conn our GuildVoice.Leave holds — so Leave can
// close exactly the right sender.
type AudioSenderRegistry struct {
	mu      sync.Mutex
	senders map[voice.Conn]*safeAudioSender
}

// NewAudioSenderRegistry creates an empty registry.
func NewAudioSenderRegistry() *AudioSenderRegistry {
	return &AudioSenderRegistry{senders: make(map[voice.Conn]*safeAudioSender)}
}

// CreateFunc returns a voice.AudioSenderCreateFunc that builds disgo's standard
// audio sender behind the safeAudioSender guard and records it against conn.
//
// disgo calls this from connImpl.SetOpusFrameProvider, which closes the
// previous sender before creating the new one — so overwriting the map entry
// mirrors what the library already did and never orphans a running sender.
func (r *AudioSenderRegistry) CreateFunc() voice.AudioSenderCreateFunc {
	return func(logger *slog.Logger, provider voice.OpusFrameProvider, conn voice.Conn) voice.AudioSender {
		g := &startSignalProvider{OpusFrameProvider: provider, started: make(chan struct{})}
		s := &safeAudioSender{
			AudioSender: voice.NewAudioSender(logger, g, conn),
			started:     g.started,
		}
		r.mu.Lock()
		r.senders[conn] = s
		r.mu.Unlock()
		return s
	}
}

// Close stops the audio sender registered for conn and forgets it. Safe to
// call for a conn that has no sender (nothing was ever registered, or it was
// already closed) and safe to call more than once.
func (r *AudioSenderRegistry) Close(conn voice.Conn) {
	r.mu.Lock()
	sender, ok := r.senders[conn]
	delete(r.senders, conn)
	r.mu.Unlock()
	if ok {
		sender.Close()
	}
}

// CloseAll stops every registered sender. Backstop for process shutdown.
func (r *AudioSenderRegistry) CloseAll() {
	r.mu.Lock()
	senders := make([]*safeAudioSender, 0, len(r.senders))
	for conn, s := range r.senders {
		senders = append(senders, s)
		delete(r.senders, conn)
	}
	r.mu.Unlock()
	for _, s := range senders {
		s.Close()
	}
}

// Opt installs this registry's create func on a bot's voice manager. Every
// client this process creates — owner, speaker pool, and the integration
// harness bots — must pass it, since any of them can be torn down while its
// gateway is already dead.
//
// voice.WithConnConfigOpts appends, so this composes with SafeUDPConnOpt and
// voice.WithDaveSessionCreateFunc rather than replacing them.
func (r *AudioSenderRegistry) Opt() voice.ManagerConfigOpt {
	return voice.WithConnConfigOpts(voice.WithConnAudioSenderCreateFunc(r.CreateFunc()))
}

// startSignalProvider passes frames through unchanged and closes started on the
// first ProvideOpusFrame call.
//
// It exists to make closing the sender safe. disgo's defaultAudioSender sets
// its cancelFunc inside the goroutine that Open spawns:
//
//	func (s *defaultAudioSender) open() {
//		...
//		ctx, cancel := context.WithCancel(context.Background())
//		s.cancelFunc = cancel   // line 75, in the new goroutine
//		...
//		s.send()                // line 84, calls ProvideOpusFrame
//	}
//
// while Close reads it from the caller's goroutine with no synchronisation:
//
//	func (s *defaultAudioSender) Close() { s.cancelFunc() }
//
// That is both a data race and a nil-func panic when Close beats the goroutine
// — the same Close-before-Open hazard safeUDPConn guards for UDP sockets, and
// join timeouts make it just as reachable here.
//
// The first ProvideOpusFrame call happens after the cancelFunc write, so
// closing started at that point and receiving from it before calling Close
// establishes a happens-before edge covering the write. Waiting on it is what
// makes safeAudioSender.Close race-free rather than merely crash-free.
type startSignalProvider struct {
	voice.OpusFrameProvider
	once    sync.Once
	started chan struct{}
}

func (p *startSignalProvider) ProvideOpusFrame() ([]byte, error) {
	p.once.Do(func() { close(p.started) })
	return p.OpusFrameProvider.ProvideOpusFrame()
}

// senderStartTimeout bounds the wait for the sender goroutine to reach its
// first ProvideOpusFrame call. Open schedules that goroutine immediately, so
// this is far more headroom than needed; it only stops teardown blocking
// forever if the goroutine never ran at all.
var senderStartTimeout = 2 * time.Second

// safeAudioSender wraps disgo's audio sender so Close is idempotent, cannot
// panic, and cannot race the sender goroutine's start-up. Everything except
// Close is promoted from the embedded interface.
type safeAudioSender struct {
	voice.AudioSender
	once    sync.Once
	started <-chan struct{}
}

// Close stops the sender goroutine. It waits for the goroutine to signal that
// it is running (see startSignalProvider) so the underlying cancelFunc read is
// ordered after its write, and recovers if disgo panics anyway.
//
// After cancellation the goroutine may emit one final "error while reading
// opus frame" line for the tick already in flight, then exits at the top of
// its loop.
func (s *safeAudioSender) Close() {
	s.once.Do(func() {
		select {
		case <-s.started:
		case <-time.After(senderStartTimeout):
			slog.Warn("voice: audio sender never started; closing anyway")
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("voice: recovered panic closing an audio sender (disgo defaultAudioSender.Close nil cancelFunc)",
					slog.Any("panic", r))
			}
		}()
		s.AudioSender.Close()
	})
}

// defaultSenders is the process-wide registry. Senders are keyed by voice.Conn,
// which is unique across every client this process builds, so one registry
// serves the owner bot and the whole speaker pool without collisions — the same
// arrangement dave.Registry uses for DAVE sessions.
var defaultSenders = NewAudioSenderRegistry()

// SafeAudioSenderOpt installs the process-wide audio-sender registry on a bot's
// voice manager. Pair it with SafeUDPConnOpt on every client, so GuildVoice.Leave
// can stop the sender that disgo's connImpl.Close leaves running.
func SafeAudioSenderOpt() voice.ManagerConfigOpt {
	return defaultSenders.Opt()
}

// CloseAudioSender stops the audio sender behind conn. Called by GuildVoice.Leave.
func CloseAudioSender(conn voice.Conn) {
	defaultSenders.Close(conn)
}

// CloseAllAudioSenders stops every tracked sender. Backstop for process shutdown.
func CloseAllAudioSenders() {
	defaultSenders.CloseAll()
}
