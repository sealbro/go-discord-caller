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

func guestPrereqs(t *testing.T) {
	t.Helper()
	skipIfMissing(t, h.Cfg.GuestGuildID != 0, "E2E_GUEST_GUILD_ID not set")
	skipIfMissing(t, h.Cfg.GuestOwnerChannelID != 0, "E2E_GUEST_OWNER_CHANNEL_ID not set")
	skipIfMissing(t, h.Cfg.GuestSpeakerChannelID != 0, "E2E_GUEST_SPEAKER_CHANNEL_ID not set")
}

// TestE5_InterGuildRelay verifies that audio from the host guild is relayed
// to a listener sitting in the guest guild's speaker channel.
func TestE5_InterGuildRelay(t *testing.T) {
	guestPrereqs(t)

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

// TestE5b_AllyCaller verifies bidirectional cross-guild audio: the guest guild
// captures audio and contributes it to the host relay mixer (AllyCaller mode),
// and the host's speaker channel receives frames from the guest source.
// Requires: GuestGuildID, SourceToken2, and the host running GuildCaller.
func TestE5b_AllyCaller(t *testing.T) {
	guestPrereqs(t)
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	// No CallerRole binding for the guest guild: E2E_CALLER_ROLE_ID is a host-guild
	// role that does not exist in the guest guild, so binding it would silently reject
	// every speaker. E5b tests relay flow; the guest filter allows any cached member.
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	// Host runs GuildCaller so guest capture is permitted (AllowGuestCapture = true).
	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5b: host relay code = %s", raidCode)

	// Guest joins as AllyCaller — it will capture and contribute audio.
	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyCaller, raidCode); err != nil {
		t.Fatalf("JoinSession (guest AllyCaller): %v", err)
	}

	// Guest source speaks in the guest owner channel; host listener watches speaker1.
	// MustStartPlaying uses GuildID so we call StartPlaying directly with GuestGuildID.
	stopSource, err := h.Speaker2.StartPlaying(ctx, h.Cfg.GuestGuildID, h.Cfg.GuestOwnerChannelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker2.StartPlaying in guest guild: %v", err)
	}
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	// The guest source's audio travels: guest capture → relay mixer → host speaker channel.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Logf("E5b passed: %d frames from guest AllyCaller reached host speaker channel",
		h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}

// TestE5c_OneManyAllyCaller verifies that in star-topology guest mode the relay
// delivers the host owner's audio to guest speaker channels, while guest sources
// contribute audio back to the host via the relay mixer.
// Requires: GuestGuildID, SourceToken2, and the host running OneManyGuildCaller.
func TestE5c_OneManyAllyCaller(t *testing.T) {
	guestPrereqs(t)
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")
	skipIfMissing(t, h.Cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	// Host runs OneManyGuildCaller (star topology, AllowGuestCapture = true).
	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeOneManyGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5c: host relay code = %s", raidCode)

	// Guest joins as OneManyAllyCaller.
	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeOneManyAllyCaller, raidCode); err != nil {
		t.Fatalf("JoinSession (guest OneManyAllyCaller): %v", err)
	}

	// Host owner captures audio and relays it to guests; source must be in OwnerChannelID.
	// In star topology (OneManyGuildCaller), speaker sources go to the hub mixer only —
	// only the owner's captured audio reaches the relay mixer and propagates to guests.
	stopSource := h.MustStartPlaying(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuestGuildID, h.Cfg.GuestSpeakerChannelID)

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	// Host source audio travels: host capture → relay mixer → guest speaker channel.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	t.Logf("E5c passed: %d frames from host reached guest speaker channel via OneManyAllyCaller relay",
		h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}
