//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// The tests in this file target one failure mode: a destination mixer that
// DrainWatcher auto-pauses during a lull and can never wake up again.
//
// Why the lull must be created by muting rather than by stopping playback:
// a source that stops leaves the voice channel, and the resulting voice-state
// event runs router.Recompute, which calls SetPaused on every destination and
// therefore repairs the pause state as a side effect. That masks the bug. The
// real-world report is a caller who *stays* in the channel and simply does not
// talk for a few seconds — no voice event, so nothing repairs the state.
//
// Why two callers: with a single caller the router picks RouteCopy, which
// bypasses the mixer entirely (raw Opus to the destination's ChOuts), so the
// mixer's pause state is irrelevant. The mixer only carries audio in RouteMix,
// which needs two or more callers.

// TestE16_HostMixResumesAfterCallerLull is the host-guild (no relay) case.
//
// Two callers share the owner's channel, so the destination is fed by its
// mixer (RouteMix). Both fall silent past DrainIdleTimeout while remaining
// connected, then speak again. The second burst must reach the speaker
// channel.
//
// With the drain latch present this fails at the second assertion: while
// paused, Mixer.collectFrames drains every input without appending to
// framesBuf, so Mixer.tick never refreshes lastActivityAt, so IdleFor() only
// grows and DrainWatcher's own SetPaused(IdleFor() > idle) condition stays
// true forever. Every frame from the resumed callers is discarded.
func TestE16_HostMixResumesAfterCallerLull(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need two callers for RouteMix")

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Both callers in-channel before raid start so prefetchChannelMembers
	// resolves them with full RoleIDs (see E1).
	stop1, mute1 := h.MustStartPlayingMutable(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stop2, mute2 := h.MustStartPlayingMutable(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	mgr := h.MustStartRaid(t, ctx, cancel, guild.RaidModeGuildCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerIDs := h.RequireSpeakers(t)
	h.RegisterCleanup(t, mgr, stop1, stop2, stopListener)

	// First burst: proves the path works and leaves the destination mixer with
	// a non-zero activity timestamp. A mixer that has never consumed a frame
	// reports IdleFor()==0 and is actively kept unpaused, so the bug cannot
	// reproduce without this step.
	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 20*time.Second)
	t.Log("E16: mix established; muting both callers to open a lull")

	// The lull. Callers stay connected — no voice event, nothing recomputes.
	mute1(true)
	mute2(true)
	time.Sleep(2 * opus.DrainIdleTimeout)

	mute1(false)
	mute2(false)
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 100, 25*time.Second)
	t.Logf("E16 passed: host mix resumed after a %s caller lull", 2*opus.DrainIdleTimeout)
}

// TestE17_RelayResumesAfterHostCallerLull is the ally case, and the one that
// matches the field report "the guest stopped hearing the host".
//
// It differs from E5d/E5e in what falls silent. Those drive audio from a guild
// that keeps capturing, so the host's relay-fed destinations stay protected by
// DrainWatcher.WithKeepAlive(relayFeed). Here the *host's own callers* go quiet
// while the guest is attached as a listener, so HasCapturingPeers is false on
// the host side and keepAlive does not hold its mixers open — they are eligible
// for exactly the auto-pause that never reverses.
func TestE17_RelayResumesAfterHostCallerLull(t *testing.T) {
	guestPrereqs(t)
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need two callers for RouteMix")

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	speakerIDs := h.RequireSpeakers(t)
	mgr, st := h.NewManager(h.Cfg.Speaker1ChannelID)

	st.BindChannel(h.Cfg.GuestGuildID, h.OwnerID, h.Cfg.GuestOwnerChannelID)
	st.BindChannel(h.Cfg.GuestGuildID, speakerIDs[0], h.Cfg.GuestSpeakerChannelID)
	st.BindRole(h.Cfg.GuestGuildID, store.RoleTypeCaller, h.Cfg.CallerRoleID)
	mgr.SeedExistingSpeakers([]snowflake.ID{h.Cfg.GuestGuildID})

	stop1, mute1 := h.MustStartPlayingMutable(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)
	stop2, mute2 := h.MustStartPlayingMutable(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
	time.Sleep(500 * time.Millisecond)

	raidCode, err := mgr.StartVoiceRaid(ctx, h.Cfg.GuildID, cancel, guild.RaidModeGuildCaller)
	if err != nil {
		t.Fatalf("StartVoiceRaid (host): %v", err)
	}
	t.Logf("E17: host relay code = %s", raidCode)

	// Guest attaches as a listener: it never captures, so the host's relay
	// keepAlive predicate stays false throughout.
	if _, err = mgr.JoinSession(ctx, h.Cfg.GuestGuildID, cancel, guild.RaidModeAllyListener, raidCode); err != nil {
		t.Fatalf("JoinSession (ally listener): %v", err)
	}

	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuestGuildID, h.Cfg.GuestSpeakerChannelID)
	t.Cleanup(func() {
		stop1()
		stop2()
		stopListener()
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuildID)
		_ = mgr.StopVoiceRaid(t.Context(), h.Cfg.GuestGuildID)
	})

	AssertFramesReceived(t, h.Listener, speakerIDs[0], 100, 25*time.Second)
	t.Log("E17: relay established; muting host callers to open a lull")

	mute1(true)
	mute2(true)
	time.Sleep(2 * opus.DrainIdleTimeout)

	mute1(false)
	mute2(false)
	AssertFramesIncreasedBy(t, h.Listener, speakerIDs[0], 100, 30*time.Second)
	t.Logf("E17 passed: relay to guest resumed after a %s host lull", 2*opus.DrainIdleTimeout)
}
