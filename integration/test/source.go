//go:build integration || stress

package test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// Speaker joins a voice channel and streams DCA audio in random order.
// It stands in for a human caller during E2E tests.
type Speaker struct {
	client *bot.Client
	id     snowflake.ID
}

func newTestSpeaker(ctx context.Context, token string) (*Speaker, error) {
	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
			pool.SafeUDPConnOpt(),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build source bot client: %w", err)
	}
	if err := client.OpenGateway(ctx); err != nil {
		return nil, fmt.Errorf("open source bot gateway: %w", err)
	}
	self, _ := client.Caches.SelfUser()
	return &Speaker{client: client, id: self.ID}, nil
}

// StartPlaying joins channelID in guildID and begins streaming .dca files from
// samplesDir in random order. Returns a cleanup func that stops playback
// and leaves the channel. The cleanup func is safe to call more than once.
func (s *Speaker) StartPlaying(ctx context.Context, guildID, channelID snowflake.ID, samplesDir string) (func(), error) {
	stop, _, err := s.StartPlayingMutable(ctx, guildID, channelID, samplesDir)
	return stop, err
}

// StartPlayingMutable is StartPlaying plus a setMuted func that silences the
// bot without disconnecting it, so a test can create a lull while the caller
// stays in the channel. See RandomFileVoiceProvider.SetMuted for why leaving
// the channel is not an acceptable substitute.
func (s *Speaker) StartPlayingMutable(ctx context.Context, guildID, channelID snowflake.ID, samplesDir string) (func(), func(bool), error) {
	paths, err := filepath.Glob(filepath.Join(samplesDir, "*.dca"))
	if err != nil || len(paths) == 0 {
		return nil, nil, fmt.Errorf("no .dca files found in %q", samplesDir)
	}
	provider, err := NewRandomFileVoiceProvider(paths)
	if err != nil {
		return nil, nil, fmt.Errorf("open dca files in %q: %w", samplesDir, err)
	}

	gv := pool.NewGuildVoice(s.client.VoiceManager, channelID)
	// Leave any previous connection for this guild before joining.
	// CreateConn returns the existing conn if one is still registered, which
	// would cause Open to hang if that conn is partially closed.
	leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	gv.Leave(leaveCtx, guildID)
	leaveCancel()

	conn, err := gv.Join(ctx, guildID)
	if err != nil {
		provider.Close()
		return nil, nil, fmt.Errorf("source bot join channel: %w", err)
	}

	conn.SetOpusFrameProvider(provider)
	conn.SetOpusFrameReceiver(opus.NewEmptyVoiceReceiver())

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		provider.Close()
		leaveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gv.Leave(leaveCtx, guildID)
		return nil, nil, fmt.Errorf("source bot set speaking: %w", err)
	}

	var stopped bool
	cleanup := func() {
		if stopped {
			return
		}
		stopped = true
		provider.Close()
		leaveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gv.Leave(leaveCtx, guildID)
	}
	return cleanup, provider.SetMuted, nil
}

// ID returns the bot's Discord user ID.
func (s *Speaker) ID() snowflake.ID { return s.id }

// Close shuts down the source bot's gateway connection.
func (s *Speaker) Close(ctx context.Context) {
	s.client.Close(ctx)
}
