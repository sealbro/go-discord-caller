//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// TestE8_BotReconnectAfterVoiceLeave verifies that when a speaker bot's voice
// connection is dropped during an active raid, the manager detects the departure
// via onVoiceLeave and calls ReconnectBotChannel to rejoin the bound channel,
// restoring audio delivery.
func TestE8_BotReconnectAfterVoiceLeave(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, sessionCancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, sessionCancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	if len(speakerIDs) == 0 {
		t.Skip("no speakers in pool")
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Drop the speaker's voice connection. Leave sends OP4 (channel_id=null) to
	// Discord, which emits VOICE_STATE_UPDATE. The owner bot's onVoiceLeave handler
	// fires → bot is detected as left → ReconnectBotChannel rejoins the bound channel.
	if _, ok := h.Pool.GetClientByID(speakerIDs[0]); !ok {
		t.Skip("speaker not connected")
	}
	leaveCtx, leaveCancel := context.WithTimeout(ctx, 5*time.Second)
	h.DisconnectSpeakerVoice(leaveCtx, h.cfg.GuildID, speakerIDs[0])
	leaveCancel()
	t.Log("E8: speaker voice connection dropped, waiting for ReconnectBotChannel...")

	baseAfterDrop := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterDrop+50 {
			t.Log("E8 passed: speaker reconnected and frames resumed after voice disconnect")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E8 failed: frames did not resume within 20 s after speaker voice disconnect")
}

// TestE9_BotReconnectAfterVoiceMove verifies that when a speaker bot is moved to
// a different voice channel during an active raid (e.g. admin drag), the manager
// detects the displacement via onVoiceMove and calls ReconnectBotChannel to return
// the bot to its bound channel, restoring audio delivery.
//
// Requires the owner bot to have the Move Members permission in the test guild.
func TestE9_BotReconnectAfterVoiceMove(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, sessionCancel := context.WithCancel(ctx)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, sessionCancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	if len(speakerIDs) == 0 {
		t.Skip("no speakers in pool")
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Use the owner bot's REST API to move the speaker into the owner channel,
	// displacing it from its bound Speaker1ChannelID. This fires GuildVoiceMove on
	// the owner bot → onVoiceMove → OnBotVoiceMove → ReconnectBotChannel.
	moveCtx, moveCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := h.MoveSpeakerVoice(moveCtx, h.cfg.GuildID, speakerIDs[0], h.cfg.OwnerChannelID); err != nil {
		moveCancel()
		t.Skipf("E9: move member failed (owner bot may lack Move Members permission): %v", err)
	}
	moveCancel()
	t.Log("E9: speaker moved to owner channel, waiting for ReconnectBotChannel...")

	baseAfterMove := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterMove+50 {
			t.Log("E9 passed: speaker reconnected and frames resumed after voice move")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E9 failed: frames did not resume within 20 s after speaker was moved to wrong channel")
}
