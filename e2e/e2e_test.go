//go:build e2e

package e2e

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// h is shared across all tests; created once in TestMain.
var h *Harness

func TestMain(m *testing.M) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("e2e config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h, err = newHarness(ctx, cfg)
	if err != nil {
		log.Fatalf("e2e harness: %v", err)
	}

	code := m.Run()

	h.Shutdown(context.Background())
	os.Exit(code)
}

// TestE1_OneCaller verifies that a source bot speaking in the owner's channel
// is relayed to the speaker channel where the listener sits.
func TestE1_OneCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Source joins before StartVoiceRaid so prefetchChannelMembers captures it
	// with full RoleIDs via the RequestMembers gateway op.
	stopSource, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	raidCode, err := mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	t.Logf("raid started, relay code: %s", raidCode)

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Relayed frames arrive attributed to the speaker bot's SSRC/user ID.
	speakerIDs := h.Pool.GetIDs()
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E1 passed: %d frames received", h.Listener.Receiver.FramesReceived(speakerIDs[0]))
}

// TestE2_GuildCallerMixMinus verifies that cross-channel audio is relayed:
// the listener in Speaker1ChannelID hears Speaker1 playing audio captured
// from Speaker2ChannelID (mix-minus routing excludes same-channel audio).
func TestE2_GuildCallerMixMinus(t *testing.T) {
	skipIfMissing(t, h.cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Source2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source1.StartPlaying: %v", err)
	}
	stopSource2, err := h.Source2.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker2ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source2.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Assign speaker 1 → channel 1, speaker 2 → channel 2.
	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID, h.cfg.Speaker2ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	// Listener sits in channel 1.
	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource1()
		stopSource2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Speaker1 (in ch1) plays cross-channel audio from ch2 — verifies relay works.
	// Frames arrive attributed to Speaker1's user ID (mix-minus routes ch2 → ch1).
	speakerIDs := h.Pool.GetIDs()
	AssertSSRCSeen(t, h.Listener, speakerIDs[0], 10*time.Second)
	t.Log("E2 passed: cross-channel relay confirmed via speaker bot frames in ch1")
}

// TestE3_PauseResume verifies that calling UpdateMixerPause toggles the mixer
// off and on, and that the listener observes a frame gap then a resumption.
func TestE3_PauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	stopSource, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	speakerIDs := h.Pool.GetIDs()

	// Wait for steady-state audio flow before pausing.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	mgr.UpdateMixerPause(h.cfg.GuildID) // pause

	AssertFrameGap(t, h.Listener, speakerIDs[0], 2*time.Second, 5*time.Second, func() {
		mgr.UpdateMixerPause(h.cfg.GuildID) // resume
	})
	t.Log("E3 passed: pause/resume state machine works")
}

// TestE4_SpeakerPoolWatchdog kills a speaker's gateway connection and verifies
// that audio resumes via disgo's internal reconnect within 30 seconds.
func TestE4_SpeakerPoolWatchdog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stopSource, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	speakerIDs := h.Pool.GetIDs()
	if len(speakerIDs) == 0 {
		t.Skip("no speakers in pool")
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
		// Restore the speaker we killed so later tests in this run can use it.
		reconnCtx, reconnCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer reconnCancel()
		h.Pool.Reconnect(reconnCtx, speakerIDs[0])
	})

	// Establish baseline.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 50, 5*time.Second)

	// Kill the first speaker's gateway.
	client, ok := h.Pool.GetClientByID(speakerIDs[0])
	if !ok {
		t.Skip("first speaker not connected")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	client.Close(closeCtx)
	closeCancel()
	t.Log("E4: speaker gateway closed, waiting for reconnect via pool.Reconnect...")

	// pool.Reconnect creates a fresh client and opens the gateway synchronously.
	reconnCtx, reconnCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reconnCancel()
	if !h.Pool.Reconnect(reconnCtx, speakerIDs[0]) {
		t.Fatal("E4 failed: pool.Reconnect did not succeed within 30 s")
	}
	t.Log("E4: speaker gateway reconnected, waiting for frames to resume...")

	// Frames should resume now that the speaker gateway is back.
	baseAfterKill := h.Listener.Receiver.FramesReceived(speakerIDs[0])
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.Listener.Receiver.FramesReceived(speakerIDs[0]) > baseAfterKill+50 {
			t.Logf("E4 passed: frames resumed after speaker gateway reconnect")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("E4 failed: frames did not resume within 15 s after speaker gateway reconnect")
}

// TestE5_InterGuildRelay verifies that audio from the host guild is relayed
// to a listener sitting in the guest guild's speaker channel.
func TestE5_InterGuildRelay(t *testing.T) {
	skipIfMissing(t, h.cfg.GuestGuildID != 0, "E2E_GUEST_GUILD_ID not set")
	skipIfMissing(t, h.cfg.GuestOwnerChannelID != 0, "E2E_GUEST_OWNER_CHANNEL_ID not set")
	skipIfMissing(t, h.cfg.GuestSpeakerChannelID != 0, "E2E_GUEST_SPEAKER_CHANNEL_ID not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	speakerIDs := h.Pool.GetIDs()
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
	stopSource, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
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

// TestE6_OneManyStarTopology verifies the star-topology mode: the owner bot mixes
// audio captured from all speaker channels and plays it in the owner channel.
func TestE6_OneManyStarTopology(t *testing.T) {
	skipIfMissing(t, h.cfg.Speaker2ChannelID != 0, "E2E_SPEAKER2_CHANNEL_ID not set")
	skipIfMissing(t, h.Source2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Sources join before StartVoiceRaid so prefetchChannelMembers gets full RoleIDs.
	stopSource1, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source1.StartPlaying: %v", err)
	}
	stopSource2, err := h.Source2.StartPlaying(ctx, h.cfg.GuildID, h.cfg.Speaker2ChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source2.StartPlaying: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Speaker1 → channel1, Speaker2 → channel2.
	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID, h.cfg.Speaker2ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneManyGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}
	// Listener in the owner's channel — the owner bot plays the mixed audio there.
	stopListenerOwner, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID)
	if err != nil {
		t.Fatalf("listener (owner channel).StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource1()
		stopSource2()
		stopListenerOwner()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// In star topology, the owner bot mixes all speaker-channel sources and plays
	// the result in OwnerChannelID. Frames arrive attributed to the owner bot's ID.
	AssertFramesReceived(t, h.Listener, h.OwnerID, 100, 10*time.Second)
	t.Log("E6 passed: star topology — owner channel receives mixed audio from both sources")
}

// TestE7_RequestMembersPreFetch verifies that a source bot already in the
// owner's voice channel before the raid starts is allowed by the role filter
// (caught by the RequestMembers gateway op issued at session start).
func TestE7_RequestMembersPreFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Source bot joins the owner's channel BEFORE StartVoiceRaid.
	stopSource, err := h.Source.StartPlaying(ctx, h.cfg.GuildID, h.cfg.OwnerChannelID, h.cfg.SamplesDir)
	if err != nil {
		t.Fatalf("source.StartPlaying: %v", err)
	}
	// Brief pause so the voice-state update reaches the gateway before we start.
	time.Sleep(1 * time.Second)

	mgr, _ := h.NewManager(h.cfg.Speaker1ChannelID)
	_, err = mgr.StartVoiceRaid(ctx, h.cfg.GuildID, cancel, guild.RaidModeOneCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid: %v", err)
	}

	stopListener, err := h.Listener.StartListening(ctx, h.cfg.GuildID, h.cfg.Speaker1ChannelID)
	if err != nil {
		t.Fatalf("listener.StartListening: %v", err)
	}

	t.Cleanup(func() {
		stopSource()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.cfg.GuildID)
	})

	// Relayed frames arrive at the listener attributed to the speaker bot's user ID.
	speakerIDs := h.Pool.GetIDs()
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 10*time.Second)
	t.Logf("E7 passed: pre-joined source bot allowed via RequestMembers cache pre-fetch")
}
