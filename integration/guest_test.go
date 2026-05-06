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
	skipIfMissing(t, h.Cfg.GuestGuildID != 0, "E2E_GUEST_GUILD_ID not set")
	skipIfMissing(t, h.Cfg.GuestOwnerChannelID != 0, "E2E_GUEST_OWNER_CHANNEL_ID not set")
	skipIfMissing(t, h.Cfg.GuestSpeakerChannelID != 0, "E2E_GUEST_SPEAKER_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	// Seed guest guild bindings in the same store.
	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	// Start host raid and capture the relay code.
	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5: host relay code = %s", raidCode)

	// Guest joins using the relay code.
	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyListener, raidCode); err != nil {
		t.Fatalf("JoinSession (guest): %v", err)
	}

	// Source speaks in the host's owner channel.
	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	// Listener is in the guest guild's speaker channel.
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuestGuildID, h.Cfg.GuestSpeakerChannelID)

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Logf("E5 passed: %d frames relayed to guest guild", h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}
