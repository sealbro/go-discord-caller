//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// closeCodeServiceRestart is a close code Discord treats as resumable, like the
// abrupt 1006 drops seen in production: the session survives on Discord's side
// and no voice-state event is sent for the bot.
const closeCodeServiceRestart = 1012

// closeCodeNormal makes disgo discard its session, so the reopened gateway
// identifies from scratch.
const closeCodeNormal = 1000

// recoveryDeadline is far below the several minutes Discord takes to reap a
// dropped session, which is the only thing that repaired the bot in production.
const recoveryDeadline = 25 * time.Second

// TestE24_SpeakerAudioRecoversAfterMainGatewayDrop reproduces a speaker bot
// that stays silent after its main gateway drops and reconnects mid-raid.
//
// In production a speaker's gateway closed with 1006 and was back within one
// watchdog tick, yet the bot kept its queue full for over four minutes and
// dropped every owner frame until Discord reaped the old session and sent a
// voice-leave event. Nothing on the speaker's side noticed the reconnect.
//
// A single caller is used so the speaker is fed by raw copy, with no mixer in
// between: the only thing under test is the speaker's own send path.
func TestE24_SpeakerAudioRecoversAfterMainGatewayDrop(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{
		{"resumable close", closeCodeServiceRestart},
		{"session discarded", closeCodeNormal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()

			stopCaller := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
			time.Sleep(500 * time.Millisecond)

			mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
			stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)
			speakerIDs := h.RequireSpeakers(t)
			h.RegisterCleanup(t, mgr, stopCaller, stopListener)

			AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 20*time.Second)

			client := mustPoolClient(t, speakerIDs[0])
			dropAndReopenGateway(t, ctx, client, tc.code)

			AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 100, recoveryDeadline)
		})
	}
}

func mustPoolClient(t testing.TB, id snowflake.ID) *bot.Client {
	t.Helper()
	client, ok := h.Pool.GetClientByID(id)
	if !ok {
		t.Fatalf("speaker %s has no connected pool client", id)
	}
	return client
}

// dropAndReopenGateway closes the bot's main gateway with code and reopens it,
// the way disgo's own reconnect loop does after a server-side drop.
func dropAndReopenGateway(t testing.TB, ctx context.Context, client *bot.Client, code int) {
	t.Helper()
	client.Gateway.CloseWithCode(ctx, code, "integration: simulated drop")
	if err := client.Gateway.Open(ctx); err != nil {
		t.Fatalf("reopen gateway: %v", err)
	}
}
