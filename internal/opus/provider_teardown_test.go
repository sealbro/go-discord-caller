package opus

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
)

// countingHandler counts ERROR records emitted by the audio sender.
type countingHandler struct {
	slog.Handler
	errors atomic.Int64
}

func (h *countingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errors.Add(1)
	}
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// countingProvider wraps an OpusFrameProvider and counts ProvideOpusFrame calls.
type countingProvider struct {
	voice.OpusFrameProvider
	inner voice.OpusFrameProvider
	calls atomic.Int64
}

func (c *countingProvider) ProvideOpusFrame() ([]byte, error) {
	c.calls.Add(1)
	return c.inner.ProvideOpusFrame()
}

func (c *countingProvider) Close() { c.inner.Close() }

// TestDisgoSpinsOnClosedProvider is a tripwire on disgo v0.19.3, not a test of
// our own behaviour.
//
// It documents the upstream half of the 2026-09-07 incident, where one speaker
// bot logged
//
//	"error while reading opus frame" / "empty voice provider closed"
//
// at a steady 50 lines per second (one per 20 ms Opus frame) for 2h50m —
// ~510k ERROR lines, and the largest CPU peak on the host.
//
// Mechanism: defaultAudioSender.send logs a provider error and returns WITHOUT
// breaking its loop (audio_sender.go:104), and connImpl.Close does not close
// the audio sender — it only closes the gateway, the UDP conn, and removes the
// conn. So when teardown closes the provider (manager.VoiceConnSetup.Apply's
// cleanup) while the sender goroutine is still alive, every subsequent 20 ms
// tick calls ProvideOpusFrame, gets an instant error, and logs it. Forever.
//
// A closed provider therefore CANNOT stop the sender by itself; only cancelling
// the sender can. That is what pool.AudioSenderRegistry does, driven from
// pool.GuildVoice.Leave — see TestAudioSenderRegistryStopsSpinningSender for
// the regression test covering the fix.
//
// When this test fails, disgo has stopped spinning on provider errors and the
// registry workaround can be deleted.
func TestDisgoSpinsOnClosedProvider(t *testing.T) {
	t.Parallel()

	p := &countingProvider{inner: NewEmptyVoiceProvider()}
	h := &countingHandler{Handler: slog.NewTextHandler(io.Discard, nil)}
	// conn is only dereferenced by send() once a frame is actually produced;
	// this provider never produces one, so nil is safe here.
	sender := voice.NewAudioSender(slog.New(h), p, nil)
	sender.Open()
	t.Cleanup(sender.Close)

	// Sender is running and parked inside ProvideOpusFrame.
	time.Sleep(100 * time.Millisecond)
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("before Close: got %d ProvideOpusFrame calls, want 1 (sender should be parked)", got)
	}

	// Teardown closes the provider, as VoiceConnSetup.Apply's cleanup does.
	p.Close()

	baseline := p.calls.Load()
	const window = 500 * time.Millisecond
	time.Sleep(window)
	after := p.calls.Load() - baseline

	// At the 20 ms frame cadence the loop yields ~25 calls per 500 ms.
	if after < 5 {
		t.Fatalf("disgo polled a closed provider only %d times in %v (%d ERROR lines) — "+
			"it no longer spins on provider errors, so pool.AudioSenderRegistry, "+
			"SafeAudioSenderOpt, and the CloseAudioSender call in GuildVoice.Leave "+
			"are obsolete and should be removed",
			after, window, h.errors.Load())
	}
}
