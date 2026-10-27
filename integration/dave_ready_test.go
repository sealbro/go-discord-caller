//go:build integration

package integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// daveReadyTimeout bounds the wait for a fresh voice connection to finish its
// DAVE handshake. A healthy join is ready within a couple of seconds.
const daveReadyTimeout = 20 * time.Second

// TestE22_SpeakerDaveReadiness records when Discord lets a speaker's DAVE
// session become ready, since a bot that is not ready sends nothing.
//
// Production, 2026-10-26: the speakers that stayed not ready for a whole raid
// were the ones bound to channels without callers — often empty, and
// server-deafened 5 s after joining. Measured here: deafening changes nothing,
// but a bot alone in its channel never becomes ready, and becomes ready as
// soon as anyone joins. Discord completes the handshake only once there is a
// group to share keys with, so a lone bot's silence is harmless — there is
// nobody to hear it.
func TestE22_SpeakerDaveReadiness(t *testing.T) {
	cases := []struct {
		name string
		// withListener puts another member in the channel before the speaker joins.
		withListener bool
		// listenerJoinsLater brings the listener in after the speaker joined alone.
		listenerJoinsLater bool
		// deafen server-deafens the speaker right after it joins.
		deafen bool
		// rejoin leaves and joins again after deafening, as the stall rejoin does.
		rejoin    bool
		wantReady bool
	}{
		{name: "with listener", withListener: true, wantReady: true},
		{name: "alone in the channel", wantReady: false},
		{name: "joined alone, then a listener arrives", listenerJoinsLater: true, wantReady: true},
		{name: "deafened after joining", withListener: true, deafen: true, wantReady: true},
		{name: "rejoined while deafened", withListener: true, deafen: true, rejoin: true, wantReady: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()

			guildID := h.Cfg.GuildID
			speakerID, gv := speakerVoice(t)

			if tc.withListener {
				stopListener := h.MustStartListening(t, ctx, guildID, gv.ChannelID())
				t.Cleanup(stopListener)
			} else {
				requireEmptyChannel(t, guildID, gv.ChannelID())
			}
			if tc.deafen {
				t.Cleanup(func() { _ = h.Listener.SetServerDeaf(guildID, speakerID, false) })
			}

			conn := mustJoin(t, ctx, gv)

			if tc.deafen {
				if err := h.Listener.SetServerDeaf(guildID, speakerID, true); err != nil {
					t.Fatalf("server-deafen speaker %s: %v", speakerID, err)
				}
			}
			if tc.listenerJoinsLater {
				time.Sleep(5 * time.Second)
				stopListener := h.MustStartListening(t, ctx, guildID, gv.ChannelID())
				t.Cleanup(stopListener)
			}
			if tc.rejoin {
				if !waitDaveReady(conn, daveReadyTimeout) {
					t.Fatalf("first join: DAVE session not ready after %v", daveReadyTimeout)
				}
				gv.Leave(ctx, guildID)
				conn = mustJoin(t, ctx, gv)
			}

			if got := waitDaveReady(conn, daveReadyTimeout); got != tc.wantReady {
				t.Fatalf("DAVE ready within %v = %v, want %v", daveReadyTimeout, got, tc.wantReady)
			}
		})
	}
}

// TestE23_LoneSpeakerIsNotReportedAsStalled is the cost of E22's lone-bot
// case. A speaker alone in its channel with speech queued is not ready, so
// every frame it skips counts toward an encrypt stall, and the stall rejoins
// it — into the same empty channel, where it is just as unready. Production,
// 2026-10-26: 1,927 such rejoins in a day, every one of them futile, for bots
// nobody could hear.
func TestE23_LoneSpeakerIsNotReportedAsStalled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	guildID := h.Cfg.GuildID
	speakerID, gv := speakerVoice(t)
	requireEmptyChannel(t, guildID, gv.ChannelID())

	var stalls atomic.Int64
	h.SetEncryptStallHandler(func(selfUserID godave.UserID, _ godave.Callbacks) {
		if string(selfUserID) == speakerID.String() {
			stalls.Add(1)
		}
	})

	conn := mustJoin(t, ctx, gv)
	speech := make(chan []byte, opus.AudioChanBuf)
	conn.SetOpusFrameProvider(opus.NewVoiceProvider(speech, noopOpusRecorder(t)))
	stopSpeech := feedSpeech(speech)
	t.Cleanup(stopSpeech)

	// Long enough for the stall detector to have fired at least once.
	const window = 15 * time.Second
	time.Sleep(window)

	if conn.DAVE().Ready() {
		t.Skip("lone speaker became DAVE-ready; Discord no longer behaves as E22 records, so there is nothing to test")
	}
	if n := stalls.Load(); n != 0 {
		t.Fatalf("lone speaker reported as encrypt-stalled %d times in %v: it would be rejoined into the same empty channel", n, window)
	}
}

// speakerVoice returns the first pool speaker and its voice handle bound to
// the speaker channel, leaving that channel when the test ends.
func speakerVoice(t *testing.T) (snowflake.ID, pool.GuildVoice) {
	t.Helper()
	speakerID := h.RequireSpeakers(t)[0]
	client, ok := h.Pool.GetClientByID(speakerID)
	if !ok {
		t.Fatalf("speaker %s has no client", speakerID)
	}
	gv := pool.NewGuildVoice(client.VoiceManager, h.Cfg.Speaker1ChannelID)
	t.Cleanup(func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		gv.Leave(leaveCtx, h.Cfg.GuildID)
	})
	return speakerID, gv
}

func mustJoin(t *testing.T, ctx context.Context, gv pool.GuildVoice) voice.Conn {
	t.Helper()
	conn, err := gv.Join(ctx, h.Cfg.GuildID)
	if err != nil {
		t.Fatalf("speaker join: %v", err)
	}
	return conn
}

// waitDaveReady reports whether conn's DAVE session becomes ready within
// timeout — the state in which disgo's sender encrypts and sends audio.
func waitDaveReady(conn voice.Conn, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn.DAVE().Ready() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// feedSpeech queues an Opus silence frame every 20 ms, as a raid's fanout
// does while someone is talking, until the returned func is called.
func feedSpeech(ch chan<- []byte) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				select {
				case ch <- opus.CopyOpusFrame([]byte{0xF8, 0xFF, 0xFE}):
				default:
				}
			}
		}
	}()
	return func() { close(done) }
}

func noopOpusRecorder(t *testing.T) telemetry.OpusRecorder {
	t.Helper()
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("integration"))
	if err != nil {
		t.Fatalf("build metrics: %v", err)
	}
	return metrics.ForGuild(context.Background(), h.Cfg.GuildID).Provider()
}

// requireEmptyChannel waits for channelID to empty — a previous test's bots
// take a moment to leave — and skips the test if someone stays, since the
// case it guards needs the speaker to be alone.
func requireEmptyChannel(t *testing.T, guildID, channelID snowflake.ID) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		occupant, occupied := channelOccupant(guildID, channelID)
		if !occupied {
			return
		}
		if time.Now().After(deadline) {
			t.Skipf("channel %s is not empty: %s is in it", channelID, occupant)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func channelOccupant(guildID, channelID snowflake.ID) (snowflake.ID, bool) {
	for vs := range h.Owner.Caches.VoiceStates(guildID) {
		if vs.ChannelID != nil && *vs.ChannelID == channelID {
			return vs.UserID, true
		}
	}
	return 0, false
}
