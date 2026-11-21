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

// guestCapturePrereqs skips a test whose guest captures: a capturing guest is
// refused without its own caller role, and the host guild's role does not
// exist in the guest guild.
func guestCapturePrereqs(t *testing.T) {
	t.Helper()
	guestPrereqs(t)
	skipIfMissing(t, h.Cfg.GuestCallerRoleID != 0, "E2E_GUEST_CALLER_ROLE_ID not set")
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

// TestE5d_RelayResumesAfterIdleHost is the regression test for issue #51 on
// the host side.
//
// The failure needs a lull, not just a quiet guild: a host channel mixer that
// has never consumed a frame reports IdleFor()==0, so DrainWatcher actively
// unpauses it every 2.5 s and relay audio flows by accident. Once the mixer HAS
// consumed frames and then sits idle past DrainIdleTimeout, DrainWatcher pauses
// it — and a paused mixer drains its inputs without recording activity, so the
// relay input can never wake it again and every packet the guest sends is
// dropped from then on.
//
// Sequence: guest talks (host mixers active) → both guilds quiet past the drain
// timeout → guest talks again. The second burst must reach the host. The guest
// caller stays connected and is muted for the lull, as in E16.
//
// The first burst is also E5b: a guest in AllyCaller mode captures its own
// caller and contributes it through the relay mixer to the host's speakers.
func TestE5d_RelayResumesAfterIdleHost(t *testing.T) {
	guestCapturePrereqs(t)
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.GuestCallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5d: host relay code = %s", raidCode)

	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyCaller, raidCode); err != nil {
		t.Fatalf("JoinSession (guest AllyCaller): %v", err)
	}

	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)
	stopSource, mute := startMutableCaller(t, ctx, h.Speaker2, h.Cfg.GuestGuildID, h.Cfg.GuestOwnerChannelID)
	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	// First burst: proves the relay path works and leaves the host's channel
	// mixers with a non-zero activity timestamp.
	phase(t, "E5b guest caller reaches host speaker channel", func(t *testing.T) {
		AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)
	})

	phase(t, "E5d relay resumes after a lull", func(t *testing.T) {
		// Lull past DrainIdleTimeout: DrainWatcher pauses the now-idle mixers.
		mute(true)
		time.Sleep(lullDuration)
		mute(false)
		AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 100, 20*time.Second)
		t.Logf("E5d: relay resumed after a %s idle window", lullDuration)
	})
}

// TestE5e_RelayResumesAfterIdleGuest is the guest-side half of E5d: the guest
// joined in a caller mode, both guilds fall silent past the drain timeout, and
// the host then speaks again. This is the configuration the reporter of #51 had
// when switching the guest from listener mode to "Many callers".
func TestE5e_RelayResumesAfterIdleGuest(t *testing.T) {
	guestPrereqs(t)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E5e: host relay code = %s", raidCode)

	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyCaller, raidCode); err != nil {
		t.Fatalf("JoinSession (guest AllyCaller): %v", err)
	}

	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuestGuildID, h.Cfg.GuestSpeakerChannelID)
	stopSource, mute := startMutableCaller(t, ctx, h.Speaker, h.Cfg.GuildID, h.Cfg.OwnerChannelID)
	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 15*time.Second)

	mute(true)
	time.Sleep(lullDuration)
	mute(false)

	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 100, 20*time.Second)
	t.Logf("E5e passed: relay resumed after a %s idle window", lullDuration)
}

// TestE5f_StarRelayResumesAfterIdleHost covers the direction E5c leaves
// untested: guest → host in star topology, across an idle lull.
//
// Star delivers guest audio ONLY to the hub — StarCallerPipeline registers the
// relay input on the hub mixer alone, and the hub's sink feeds the owner bot's
// provider — so the listener sits in the HOST's owner channel and counts frames
// from the owner bot, not from a speaker.
//
// This is the star half of the #51 fix (hub RelayFeed + DrainWatcher keep-alive);
// E5d/E5e only exercise the GuildCaller/AllyCaller pipelines.
func TestE5f_StarRelayResumesAfterIdleHost(t *testing.T) {
	guestCapturePrereqs(t)
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)

	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.GuestCallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeOneManyGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host star): %v", err)
	}
	t.Logf("E5f: host relay code = %s", raidCode)

	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeOneManyAllyCaller, raidCode); err != nil {
		t.Fatalf("JoinSession (guest OneManyAllyCaller): %v", err)
	}

	// The hub mix is played by the owner bot into the host's owner channel.
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.OwnerChannelID)
	stopSource, mute := startMutableCaller(t, ctx, h.Speaker2, h.Cfg.GuestGuildID, h.Cfg.GuestOwnerChannelID)
	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 15*time.Second)

	mute(true)
	time.Sleep(lullDuration)
	mute(false)

	AssertFramesIncreasedBy(t, h.Listener, h.OwnerID, 100, 20*time.Second)
	t.Logf("E5f passed: star hub relay resumed after a %s idle window", lullDuration)
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
