//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/guild"
)

// routeSwitchWait covers the router's 250 ms debounce plus the Discord event
// that triggers it.
const routeSwitchWait = 5 * time.Second

// TestE13_E15_E14_AutoRouteCopyMixCopy walks the auto-router through both
// transitions on one raid, asserting each on the router's own transition
// counter as well as on delivery:
//
//   - E13: one caller opens in copy mode; a second caller joining promotes the
//     source to mix without a catastrophic gap (250 ms debounce + atomic
//     install swap → well under a second).
//   - E15: two users in one source channel get distinct SourceBuffers, so the
//     cap-3 ring does not silently evict one. Spectrogram analysis is out of
//     scope, so it asserts the stronger invariant — sustained mixer throughput
//     under two-caller load.
//   - E14: revoking the second caller's role demotes mix back to copy. A bot
//     leaving would not do it: onVoiceLeave returns before AutoRoute for bots,
//     and every caller here is a bot, so the demotion is driven through
//     onGuildMemberUpdate → NotifyMemberUpdate instead.
func TestE13_E15_E14_AutoRouteCopyMixCopy(t *testing.T) {
	skipIfMissing(t, h.Speaker2 != nil, "E2E_SOURCE_BOT_TOKEN_2 not set — need a second caller for mix mode")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stopSpeaker1 := startCaller(t, ctx, h.Speaker, h.Cfg.OwnerChannelID)

	_, sessionCancel := context.WithCancel(ctx)
	mgr, reader := h.MustStartRaidRecording(t, ctx, sessionCancel, guild.RaidModeOneCaller, h.Cfg.Speaker1ChannelID)
	stopListener := h.MustStartListening(t, ctx, h.Cfg.GuildID, h.Cfg.Speaker1ChannelID)

	speakerID := h.RequireSpeakers(t)[0]

	stopSpeaker2 := func() {}
	t.Cleanup(func() {
		stopSpeaker1()
		stopSpeaker2()
		stopListener()
		_ = mgr.StopVoiceRaid(context.Background(), h.Cfg.GuildID)
	})

	phase(t, "E13 single caller in copy mode", func(t *testing.T) {
		awaitRouteTransition(t, reader, "off", "copy", 0, routeSwitchWait)
		AssertFramesReceived(t, h.Listener, speakerID, 50, 8*time.Second)
	})

	phase(t, "E13 second caller promotes copy to mix", func(t *testing.T) {
		base := routeTransitions(t, reader, "copy", "mix")
		stopSpeaker2 = startCaller(t, ctx, h.Speaker2, h.Cfg.OwnerChannelID)
		awaitRouteTransition(t, reader, "copy", "mix", base, routeSwitchWait)
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 5*time.Second)
	})

	phase(t, "E15 two callers sustain mix throughput", func(t *testing.T) {
		// 400 frames / 10 s ≈ 80 % of nominal 50 fps. The pre-fix shared ring
		// would lose about half of that under continuous two-caller load.
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 400, 10*time.Second)
	})

	scenario := t
	phase(t, "E14 role revoke demotes mix to copy", func(t *testing.T) {
		requireRoleManagement(t)
		base := routeTransitions(t, reader, "mix", "copy")
		revokeCallerRole(scenario, h.Speaker2.ID())
		awaitRouteTransition(t, reader, "mix", "copy", base, routeSwitchWait)
		AssertFramesIncreasedBy(t, h.Listener, speakerID, 50, 5*time.Second)
	})
}
