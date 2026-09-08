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

// TestClosedProviderDoesNotSpinAudioSender reproduces the production incident of
// 2026-09-07, where one speaker bot logged
//
//	"error while reading opus frame" / "empty voice provider closed"
//
// at a steady 50 lines per second (one per 20 ms Opus frame) for 2h50m —
// ~510k ERROR lines, and the largest CPU peak on the host.
//
// Mechanism: disgo's defaultAudioSender.send logs a provider error and returns
// WITHOUT breaking its loop (audio_sender.go:104), and connImpl.Close does not
// close the audio sender — it only closes the gateway, the UDP conn, and
// removes the conn. So when teardown closes the provider (VoiceConnSetup.Apply's
// cleanup) while the sender goroutine is still alive, every subsequent 20 ms
// tick calls ProvideOpusFrame, gets an instant error, and logs it. Forever.
//
// The invariant this asserts: once a provider is closed, it must not keep
// feeding the sender an error on every tick. A closed provider is a torn-down
// provider; it has no work left to report.
func TestClosedProviderDoesNotSpinAudioSender(t *testing.T) {
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

	// At the 20 ms frame cadence an unbounded loop yields ~25 calls per 500 ms.
	// Allow a couple for the in-flight tick that raced Close.
	const tolerance = 3
	if after > tolerance {
		t.Fatalf("closed provider was polled %d times in %v (%.0f calls/sec), "+
			"producing %d ERROR log lines — the audio sender is spinning on a "+
			"closed provider and logging on every tick; want at most %d calls",
			after, window, float64(after)/window.Seconds(), h.errors.Load(), tolerance)
	}
}
