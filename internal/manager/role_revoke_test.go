package manager

import (
	"context"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// A caller whose role is taken away mid-raid must stop being relayed at once,
// not when they next leave or rejoin voice.
func TestRoleRevokedDuringRaidStopsCapture(t *testing.T) {
	const callerID = snowflake.ID(42)
	f := withCallerRole(newCallerRoleFixture(t))
	putInVoice(f, callerID, roleTestChannels[testGuildID][0], false, roleTestCallerRoleID)

	_, cancel, err := startHostRaid(t, f)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cancel()
	defer func() { _ = f.m.StopVoiceRaid(context.Background(), testGuildID) }()

	f.m.mu.RLock()
	filter, _ := f.m.statuses[testGuildID].Session.AllowFilter.(*AllowFilter)
	f.m.mu.RUnlock()
	if filter == nil {
		t.Fatal("session has no allow filter")
	}
	if !filter.Check(callerID) {
		t.Fatal("member holding the caller role is not relayed before the revoke")
	}

	revoked := discord.Member{GuildID: testGuildID, User: discord.User{ID: callerID}}
	f.caches.AddMember(revoked)
	f.m.NotifyMemberUpdate(testGuildID, revoked)

	if filter.Check(callerID) {
		t.Error("caller role revoked mid-raid, but the member is still relayed")
	}
}
