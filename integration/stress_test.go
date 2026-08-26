//go:build stress

package integration

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// stressDuration returns how long a stress test should run. STRESS_DURATION
// overrides the test's default (any Go duration string, e.g. "90m", "2h"), so a
// listen-by-ear session can be stretched or cut short without editing code.
//
// Remember to raise `go test -timeout` to match — the Makefile targets do this
// automatically.
func stressDuration(t *testing.T, def time.Duration) time.Duration {
	t.Helper()

	raw := os.Getenv("STRESS_DURATION")
	if raw == "" {
		return def
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("STRESS_DURATION=%q: %v", raw, err)
	}
	if d <= 0 {
		t.Fatalf("STRESS_DURATION=%q: must be positive", raw)
	}

	return d
}

// TestStress_AllBotsPlayAudio connects all three harness bots to voice channels and plays
// audio files for a fixed duration so the output can be verified by ear. The bot code
// (manager/raid) is not started — run it manually before executing this test.
//
// Bot assignments:
//   - E2E_SOURCE_BOT_TOKEN   → E2E_SPEAKER_CHANNEL_ID
//   - E2E_SOURCE_BOT_TOKEN_2 → E2E_SPEAKER2_CHANNEL_ID
//   - E2E_LISTENER_BOT_TOKEN → E2E_SPEAKER_CHANNEL_ID
//
// Run explicitly:
//
//	make test-stress-audio                      # default 50m
//	make test-stress-audio STRESS_DURATION=2h   # listen for two hours
func TestStress_AllBotsPlayAudio(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	runDuration := stressDuration(t, 50*time.Minute)

	ctx, cancel := context.WithTimeout(t.Context(), runDuration+30*time.Second)
	defer cancel()

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	defer stopSource1()
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker1ChannelID)
	defer stopSource2()
	stopListener := h.MustStartPlayingListener(t, ctx, h.Cfg.OwnerChannelID)
	defer stopListener()

	t.Logf("all 3 bots playing audio for %s in guild %s — verify quality by ear", runDuration, h.Cfg.GuildID)

	select {
	case <-ctx.Done():
	case <-time.After(runDuration):
	}

	t.Log("TestStress_AllBotsPlayAudio complete")
}

// TestStress_OneManyStarTopologyLong runs the star-topology scenario for a
// configurable duration (default 5 minutes) and checks for audio quality regressions that manifest as robotic or choppy voice.
//
// Every second the test samples the owner-channel frame counter. At the end of each
// 30-second window it checks:
//   - at least 80 % of expected frames arrived (dropout / silence detection)
//   - no per-second gap longer than 2 s (short silence burst detection)
//
// Run explicitly:
//
//	make test-stress-star                        # default 5m
//	make test-stress-star STRESS_DURATION=30m    # 30-minute stability run
func TestStress_OneManyStarTopologyLong(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	runDuration := stressDuration(t, 5*time.Minute)

	const (
		windowDuration  = 30 * time.Second
		sampleInterval  = 1 * time.Second
		minWindowFrames = int64(1200) // 80 % of ~1500 expected (50 fps × 30 s)
		maxSilenceSecs  = 2           // consecutive silent seconds before fail
	)

	ctx, cancel := context.WithTimeout(t.Context(), runDuration+30*time.Second)
	defer cancel()

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeOneManyGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListenerOwner := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID)

	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListenerOwner)

	// Wait for steady-state audio before the measurement window begins.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Logf("E8: steady state reached, starting %s quality observation", runDuration)

	// Collect per-second frame deltas in a background goroutine.
	type sample struct{ delta int64 }
	var (
		samplesMu sync.Mutex
		samples   []sample
	)
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		prev := h.Listener.Receiver.FramesReceived(h.OwnerID)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := h.Listener.Receiver.FramesReceived(h.OwnerID)
				samplesMu.Lock()
				samples = append(samples, sample{now - prev})
				samplesMu.Unlock()
				prev = now
			}
		}
	}()

	totalWindows := int(runDuration / windowDuration)
	samplesPerWindow := int(windowDuration / sampleInterval) // 30
	for w := 0; w < totalWindows; w++ {
		time.Sleep(windowDuration)

		samplesMu.Lock()
		start := w * samplesPerWindow
		end := start + samplesPerWindow
		if end > len(samples) {
			end = len(samples)
		}
		window := make([]sample, end-start)
		copy(window, samples[start:end])
		samplesMu.Unlock()

		var windowTotal int64
		maxConsecSilence := 0
		consecSilence := 0
		for _, s := range window {
			windowTotal += s.delta
			if s.delta == 0 {
				consecSilence++
				if consecSilence > maxConsecSilence {
					maxConsecSilence = consecSilence
				}
			} else {
				consecSilence = 0
			}
		}

		t.Logf("window %2d/%d: +%d frames  max_silence_gap=%ds",
			w+1, totalWindows, windowTotal, maxConsecSilence)

		if windowTotal < minWindowFrames {
			t.Errorf("window %d: only %d frames in %s (expected >= %d) — dropout or sustained silence (robotic voice symptom)",
				w+1, windowTotal, windowDuration, minWindowFrames)
		}
		if maxConsecSilence >= maxSilenceSecs {
			t.Errorf("window %d: %d consecutive silent seconds — short silence burst that causes robotic voice",
				w+1, maxConsecSilence)
		}
	}

	total := h.Listener.Receiver.FramesReceived(h.OwnerID)
	t.Logf("E8 passed: %d total frames over %s in OneManyStarTopology", total, runDuration)
}

// TestStress_GuildCallerMixMinusLong runs the mix-minus scenario for a
// configurable duration (default 5 minutes) and checks for audio quality regressions that manifest as robotic or choppy voice.
//
// Every second the test samples the speaker1-channel frame counter. At the end of each
// 30-second window it checks:
//   - at least 80 % of expected frames arrived (dropout / silence detection)
//   - no per-second gap longer than 2 s (short silence burst detection)
//
// Run explicitly:
//
//	make test-stress-mixminus                        # default 5m
//	make test-stress-mixminus STRESS_DURATION=30m    # 30-minute stability run
func TestStress_GuildCallerMixMinusLong(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	runDuration := stressDuration(t, 5*time.Minute)

	const (
		windowDuration  = 30 * time.Second
		sampleInterval  = 1 * time.Second
		minWindowFrames = int64(1200) // 80 % of ~1500 expected (50 fps × 30 s)
		maxSilenceSecs  = 2           // consecutive silent seconds before fail
	)

	ctx, cancel := context.WithTimeout(t.Context(), runDuration+30*time.Second)
	defer cancel()

	stopSource1 := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.Speaker1ChannelID)
	stopSource2 := h.MustStartPlaying(t, ctx, h.Speaker2, h.Cfg.Speaker2ChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stopSource1, stopSource2, stopListener)

	// Wait for steady-state audio before the measurement window begins.
	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Logf("GuildCallerMixMinusLong: steady state reached, starting %s quality observation", runDuration)

	// Collect per-second frame deltas in a background goroutine.
	type sample struct{ delta int64 }
	var (
		samplesMu sync.Mutex
		samples   []sample
	)
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		prev := h.Listener.Receiver.FramesReceived(speakerIDs[0])
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := h.Listener.Receiver.FramesReceived(speakerIDs[0])
				samplesMu.Lock()
				samples = append(samples, sample{now - prev})
				samplesMu.Unlock()
				prev = now
			}
		}
	}()

	totalWindows := int(runDuration / windowDuration)
	samplesPerWindow := int(windowDuration / sampleInterval) // 30
	for w := 0; w < totalWindows; w++ {
		time.Sleep(windowDuration)

		samplesMu.Lock()
		start := w * samplesPerWindow
		end := start + samplesPerWindow
		if end > len(samples) {
			end = len(samples)
		}
		window := make([]sample, end-start)
		copy(window, samples[start:end])
		samplesMu.Unlock()

		var windowTotal int64
		maxConsecSilence := 0
		consecSilence := 0
		for _, s := range window {
			windowTotal += s.delta
			if s.delta == 0 {
				consecSilence++
				if consecSilence > maxConsecSilence {
					maxConsecSilence = consecSilence
				}
			} else {
				consecSilence = 0
			}
		}

		t.Logf("window %2d/%d: +%d frames  max_silence_gap=%ds",
			w+1, totalWindows, windowTotal, maxConsecSilence)

		if windowTotal < minWindowFrames {
			t.Errorf("window %d: only %d frames in %s (expected >= %d) — dropout or sustained silence (robotic voice symptom)",
				w+1, windowTotal, windowDuration, minWindowFrames)
		}
		if maxConsecSilence >= maxSilenceSecs {
			t.Errorf("window %d: %d consecutive silent seconds — short silence burst that causes robotic voice",
				w+1, maxConsecSilence)
		}
	}

	total := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	t.Logf("GuildCallerMixMinusLong passed: %d total frames over %s in GuildCallerMixMinus", total, runDuration)
}
