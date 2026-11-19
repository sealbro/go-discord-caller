//go:build stress

package integration

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"github.com/sealbro/go-discord-caller/internal/pool"
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

// stressCycles returns how many cycles a cycling stress test should run;
// STRESS_CYCLES overrides the test's default.
func stressCycles(t *testing.T, def int) int {
	t.Helper()

	raw := os.Getenv("STRESS_CYCLES")
	if raw == "" {
		return def
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("STRESS_CYCLES=%q: must be a positive integer", raw)
	}

	return n
}

// TestStress_StartCancelStart repeats what a production guild did before every
// join in it hung: /start, /stop while the bots were still joining, and /start
// again straight away. Each cycle cancels at a different point of the voice
// handshake, and the restart that follows must bring every bot in.
//
// Run explicitly:
//
//	make test-stress-start-cancel                    # default 20 cycles
//	make test-stress-start-cancel STRESS_CYCLES=100
func TestStress_StartCancelStart(t *testing.T) {
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	speakerIDs := h.RequireSpeakers(t)

	const (
		mode        = guild.RaidModeOneManyGuildCaller
		cancelStep  = 250 * time.Millisecond
		cancelSteps = 8 // cancels sweep 0–1.75 s, across the whole handshake
		cycleGap    = time.Second
	)
	cycles := stressCycles(t, 20)
	guildID := h.Cfg.GuildID

	channels := []snowflake.ID{h.Cfg.Speaker1ChannelID, h.Cfg.Speaker2ChannelID}
	mgr, _ := h.NewManager(channels...)
	t.Cleanup(func() { _ = mgr.StopVoiceRaid(context.Background(), guildID) })

	wantInVoice := map[snowflake.ID]snowflake.ID{h.OwnerID: h.Cfg.OwnerChannelID}
	for i, id := range speakerIDs[:min(len(speakerIDs), len(channels))] {
		wantInVoice[id] = channels[i]
	}
	wantSpeakers := len(wantInVoice) - 1

	stop := func(cycle int) {
		t.Helper()
		if err := mgr.StopVoiceRaid(t.Context(), guildID); err != nil && !errors.Is(err, manager.ErrNoActiveSession) {
			t.Fatalf("cycle %d: StopVoiceRaid: %v", cycle, err)
		}
	}

	for cycle := range cycles {
		cancelAfter := time.Duration(cycle%cancelSteps) * cancelStep

		firstCtx, firstCancel := context.WithCancel(t.Context())
		firstDone := make(chan error, 1)
		go func() {
			_, err := mgr.StartVoiceRaid(firstCtx, guildID, firstCancel, mode)
			firstDone <- err
		}()
		time.Sleep(cancelAfter)
		stop(cycle)
		var firstErr error
		select {
		case firstErr = <-firstDone:
		case <-time.After(2 * pool.VoiceJoinCost):
			t.Fatalf("cycle %d: start cancelled after %s never returned", cycle, cancelAfter)
		}
		firstCancel()
		// A stop that lands before the start registers finds nothing to cancel,
		// and that start commits; stopping again leaves the guild idle either way.
		stop(cycle)

		ctx, cancel := context.WithCancel(t.Context())
		begin := time.Now()
		_, err := mgr.StartVoiceRaid(ctx, guildID, cancel, mode)
		took := time.Since(begin)
		if err != nil {
			cancel()
			t.Fatalf("cycle %d: restart after a start cancelled at %s (first start: %v) failed after %s: %v",
				cycle, cancelAfter, firstErr, took, err)
		}

		var joined int
		if s := mgr.GetStatus(guildID).Session; s != nil {
			joined = len(s.Speakers)
		}
		if joined != wantSpeakers {
			t.Errorf("cycle %d: restart after a start cancelled at %s joined %d of %d speakers",
				cycle, cancelAfter, joined, wantSpeakers)
		}
		for botID, channelID := range wantInVoice {
			if !waitInVoice(guildID, botID, channelID, 5*time.Second) {
				t.Errorf("cycle %d: bot %s not in its channel after the restart", cycle, botID)
			}
		}
		t.Logf("cycle %2d/%d: cancel after %-6s first start: %-20v restart %s, %d/%d speakers",
			cycle+1, cycles, cancelAfter, firstErr, took.Round(time.Millisecond), joined, wantSpeakers)

		stop(cycle)
		cancel()
		if t.Failed() {
			return
		}
		time.Sleep(cycleGap)
	}
}

// waitInVoice reports whether the owner bot's cache shows botID in channelID
// within timeout.
func waitInVoice(guildID, botID, channelID snowflake.ID, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if vs, ok := h.Owner.Caches.VoiceState(guildID, botID); ok && vs.ChannelID != nil && *vs.ChannelID == channelID {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
