//go:build integration

package test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// CountingReceiver is a voice.OpusFrameReceiver that counts incoming frames per
// source userID and records the timestamp of the first frame per source.
// All methods are safe for concurrent use.
type CountingReceiver struct {
	counts     sync.Map // snowflake.ID → *atomic.Int64
	firstFrame sync.Map // snowflake.ID → time.Time
	done       chan struct{}
	once       sync.Once
}

func NewCountingReceiver() *CountingReceiver {
	return &CountingReceiver{done: make(chan struct{})}
}

func (r *CountingReceiver) ReceiveOpusFrame(userID snowflake.ID, pkt *voice.Packet) error {
	if pkt == nil {
		return nil
	}
	select {
	case <-r.done:
		return nil
	default:
	}

	// Increment frame counter.
	v, _ := r.counts.LoadOrStore(userID, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)

	// Record first-frame timestamp once per userID.
	r.firstFrame.LoadOrStore(userID, time.Now())
	return nil
}

func (r *CountingReceiver) CleanupUser(_ snowflake.ID) {}

func (r *CountingReceiver) Close() {
	r.once.Do(func() { close(r.done) })
}

// FramesReceived returns the number of Opus frames received from userID.
func (r *CountingReceiver) FramesReceived(userID snowflake.ID) int64 {
	v, ok := r.counts.Load(userID)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// FirstFrameAt returns the wall-clock time of the first frame from userID.
func (r *CountingReceiver) FirstFrameAt(userID snowflake.ID) (time.Time, bool) {
	v, ok := r.firstFrame.Load(userID)
	if !ok {
		return time.Time{}, false
	}
	return v.(time.Time), true
}

// Reset clears all frame counts and timestamps. Use between test cases.
func (r *CountingReceiver) Reset() {
	r.counts.Range(func(k, _ any) bool {
		r.counts.Delete(k)
		return true
	})
	r.firstFrame.Range(func(k, _ any) bool {
		r.firstFrame.Delete(k)
		return true
	})
}

// Listener joins a voice channel and counts incoming Opus frames per source userID.
type Listener struct {
	client   *bot.Client
	id       snowflake.ID
	Receiver *CountingReceiver
}

func newTestListener(ctx context.Context, token string) (*Listener, error) {
	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build listener bot client: %w", err)
	}
	if err := client.OpenGateway(ctx); err != nil {
		return nil, fmt.Errorf("open listener bot gateway: %w", err)
	}
	self, _ := client.Caches.SelfUser()
	return &Listener{
		client:   client,
		id:       self.ID,
		Receiver: NewCountingReceiver(),
	}, nil
}

// StartListening joins channelID in guildID and begins collecting frames.
// Returns a cleanup func that stops listening and leaves the channel.
// Resets the frame counters before joining so each test starts clean.
func (l *Listener) StartListening(ctx context.Context, guildID, channelID snowflake.ID) (func(), error) {
	// Fresh receiver per listening session so prior frames don't bleed in.
	l.Receiver = NewCountingReceiver()

	gv := pool.NewGuildVoice(l.client.VoiceManager, channelID)
	conn, err := gv.Join(ctx, guildID)
	if err != nil {
		return nil, fmt.Errorf("listener bot join channel: %w", err)
	}

	conn.SetOpusFrameReceiver(l.Receiver)
	conn.SetOpusFrameProvider(opus.NewEmptyVoiceProvider())

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		l.Receiver.Close()
		leaveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gv.Leave(leaveCtx, guildID)
		return nil, fmt.Errorf("listener bot set speaking: %w", err)
	}

	var stopped bool
	cleanup := func() {
		if stopped {
			return
		}
		stopped = true
		l.Receiver.Close()
		leaveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gv.Leave(leaveCtx, guildID)
	}
	return cleanup, nil
}

// GetMember fetches a guild member via the listener bot's REST client.
// The listener bot is the designated test-admin bot (Administrator role in the
// test guild) so privileged setup calls go through it, keeping the owner bot at
// production-identical permissions.
func (l *Listener) GetMember(guildID, userID snowflake.ID) (*discord.Member, error) {
	return l.client.Rest.GetMember(guildID, userID)
}

// UpdateMember modifies a guild member via the listener bot's REST client.
// See GetMember for why this goes through the listener bot.
func (l *Listener) UpdateMember(guildID, userID snowflake.ID, update discord.MemberUpdate) (*discord.Member, error) {
	return l.client.Rest.UpdateMember(guildID, userID, update)
}

// Close shuts down the listener bot's gateway connection.
func (l *Listener) Close(ctx context.Context) {
	l.client.Close(ctx)
}
