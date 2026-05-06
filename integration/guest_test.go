//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// TestE5_InterGuildRelay verifies that audio from the host guild is relayed
// to a listener sitting in the guest guild's speaker channel.
func TestE5_InterGuildRelay(t *testing.T) {
	skipIfMissing(t, h.cfg.GuestGuildID != 0, "E2E_GUEST_GUILD_ID not set")
	skipIfMissing(t, h.cfg.GuestOwnerChannelID != 0, "E2E_GUEST_OWNER_CHANNEL_ID not set")
	skipIfMissing(t, h.cfg.GuestSpeakerChannelID != 0, "E2E_GUEST_SPEAKER_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	speakerIDs := h.Pool.ConnectedSpeakerIDs()
	if len(speakerIDs) == 0 {
		t.Skip("no speakers in pool")
	}

	mgr, st := h.NewManager(h.cfg.Speaker1ChannelID)

	// Seed guest guild bindings in the same store.
	st.BindChannel(h.cfg.GuestGuildID, h.OwnerID, h.cfg.GuestOwnerChannelID)
	st.BindChannel(h.cfg.GuestGuildID, speakerIDs[0], h.cfg.GuestSpeakerChannelID)
	st.BindRole(h.cfg.GuestGuildID, store.RoleTypeCaller, h.cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.cfg.GuestGuildID})

	// Start host raid and capture the relay code.
	raidCode, err := mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5: host relay code = %s", raidCode)

	// Guest joins using the relay code.
	_, err = mgr.JoinSession(ctx, h.cfg.GuestGuildID, cancel, guild.RaidModeAllyListener, raidCode)
	if err != nil {
		t.Fatalf("JoinSession (guest): %v", err)
	}

	// Source speaks in the host's owner channel.
	stopSource, err := h.Speaker.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	// Listener is in the guest guild's speaker channel.
	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuestGuildID, h.cfg.GuestSpeakerChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening (guest): %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuestGuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Logf("E5 passed: %d frames relayed to guest guild", h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}
