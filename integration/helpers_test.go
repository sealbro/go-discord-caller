//go:build integration || stress

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/integration/test"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// voiceStateWait bounds how long a joined bot may take to show up in the owner
// bot's voice-state cache. The event normally lands within a second.
const voiceStateWait = 10 * time.Second

// phase runs one step of a scenario as a subtest and stops the scenario when
// it fails: every later step builds on the state this one was meant to reach.
func phase(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}

// awaitInVoice waits until the owner bot's cache shows userID in channelID.
// A raid prefetches the members already in its channels, so a caller must be
// visible there before the raid starts or it is only picked up by the live
// join path.
func awaitInVoice(t testing.TB, guildID, userID, channelID snowflake.ID) {
	t.Helper()
	deadline := time.Now().Add(voiceStateWait)
	for {
		if vs, ok := h.Owner.Caches.VoiceState(guildID, userID); ok && vs.ChannelID != nil && *vs.ChannelID == channelID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear in voice channel %s within %v", userID, channelID, voiceStateWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startCaller starts speaker playing in channelID of the test guild and waits
// until the owner bot sees it there.
func startCaller(t testing.TB, ctx context.Context, speaker *test.Speaker, channelID snowflake.ID) func() {
	t.Helper()
	stop := h.MustStartPlaying(t, ctx, speaker, channelID)
	awaitInVoice(t, h.Cfg.GuildID, speaker.ID(), channelID)
	return stop
}

// startMutableCaller is startCaller with a mute func, for a lull in which the
// caller stays connected.
func startMutableCaller(t testing.TB, ctx context.Context, speaker *test.Speaker, guildID, channelID snowflake.ID) (func(), func(bool)) {
	t.Helper()
	stop, setMuted, err := speaker.StartPlayingMutable(ctx, guildID, channelID, h.Cfg.SamplesDir)
	if err != nil {
		t.Fatalf("speaker.StartPlayingMutable: %v", err)
	}
	awaitInVoice(t, guildID, speaker.ID(), channelID)
	return stop, setMuted
}

// requireRoleManagement skips the test when the listener (the harness's admin
// bot) cannot grant and revoke the caller role.
func requireRoleManagement(t testing.TB) {
	t.Helper()
	canManage, why, err := h.Listener.CanManageRole(h.Cfg.GuildID, h.Cfg.CallerRoleID)
	if err != nil {
		t.Fatalf("check caller-role management: %v", err)
	}
	skipIfMissing(t, canManage, "cannot manage the caller role: %s", why)
}

// revokeCallerRole takes the caller role from userID mid-raid and gives it
// back when the test ends, so later tests still see a caller.
func revokeCallerRole(t testing.TB, userID snowflake.ID) {
	t.Helper()
	guildID, roleID := h.Cfg.GuildID, h.Cfg.CallerRoleID
	t.Cleanup(func() { _ = h.Listener.AddMemberRole(guildID, userID, roleID) })
	if err := h.Listener.RemoveMemberRole(guildID, userID, roleID); err != nil {
		t.Fatalf("revoke the caller role from %s: %v", userID, err)
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

// routeTransitions is how many from→to source-mode transitions the raid's
// router has recorded so far.
func routeTransitions(t testing.TB, reader *sdkmetric.ManualReader, from, to string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.session.route_transitions.total" {
				continue
			}
			for _, dp := range md.Data.(metricdata.Sum[int64]).DataPoints {
				gotFrom, _ := dp.Attributes.Value("from")
				gotTo, _ := dp.Attributes.Value("to")
				if gotFrom.AsString() == from && gotTo.AsString() == to {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// awaitRouteTransition waits until the router records one more from→to
// transition than base.
func awaitRouteTransition(t testing.TB, reader *sdkmetric.ManualReader, from, to string, base int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := routeTransitions(t, reader, from, to)
		if n > base {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("router recorded no %s→%s transition within %v", from, to, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
