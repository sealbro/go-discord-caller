package manager

import (
	"context"
	"slices"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

const (
	viewTestOtherRole snowflake.ID = 760
	viewTestEmptyCh   snowflake.ID = 761
)

// putInVoice caches userID as a member of testGuildID holding roles, in
// channelID. A nil roles slice leaves the member out of the member cache.
func putInVoice(f *callerRoleFixture, userID, channelID snowflake.ID, bot bool, roles ...snowflake.ID) {
	if roles != nil {
		f.caches.AddMember(discord.Member{GuildID: testGuildID, User: discord.User{ID: userID, Bot: bot}, RoleIDs: roles})
	}
	f.caches.AddVoiceState(discord.VoiceState{GuildID: testGuildID, ChannelID: &channelID, UserID: userID})
}

func TestViewProbe_AnswersLikeTheCaches(t *testing.T) {
	chA, chB := roleTestChannels[testGuildID][0], roleTestChannels[testGuildID][1]
	for _, allowBots := range []bool{false, true} {
		f := newCallerRoleFixture(t)
		f.m.test.AllowBots = allowBots
		putInVoice(f, 751, chA, false, roleTestCallerRoleID)
		putInVoice(f, 752, chA, false, viewTestOtherRole)
		putInVoice(f, 753, chA, true, roleTestCallerRoleID)
		putInVoice(f, 754, chB, false) // in voice, not in the member cache
		putInVoice(f, 755, chB, false, roleTestCallerRoleID, viewTestOtherRole)

		view := f.m.openGuildView(testGuildID)
		cache := &cacheVoiceProbe{svc: f.m, guildID: testGuildID}

		for _, ch := range []snowflake.ID{chA, chB, viewTestEmptyCh} {
			if got, want := view.HasListeners(ch), cache.HasListeners(ch); got != want {
				t.Errorf("allowBots=%v HasListeners(%d) = %v, cache says %v", allowBots, ch, got, want)
			}
			for _, role := range []snowflake.ID{roleTestCallerRoleID, viewTestOtherRole, 0} {
				got, want := view.EnumerateCallers(ch, role), cache.EnumerateCallers(ch, role)
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("allowBots=%v EnumerateCallers(%d, %d) = %v, cache says %v", allowBots, ch, role, got, want)
				}
			}
		}

		// The comparison only means something if the cases differ.
		wantCallersA := []snowflake.ID{751}
		if allowBots {
			wantCallersA = []snowflake.ID{751, 753}
		}
		gotCallersA := view.EnumerateCallers(chA, roleTestCallerRoleID)
		slices.Sort(gotCallersA)
		if !slices.Equal(gotCallersA, wantCallersA) {
			t.Errorf("allowBots=%v callers in A = %v, want %v", allowBots, gotCallersA, wantCallersA)
		}
	}
}

func TestRefreshVoiceView_EveryEntryPointCatchesUp(t *testing.T) {
	channel := roleTestChannels[testGuildID][0]
	const caller snowflake.ID = 756
	entryPoints := []struct {
		name    string
		trigger func(m *Service)
	}{
		{"voice-state event", func(m *Service) { m.RefreshVoiceView(testGuildID) }},
		{"auto-route", func(m *Service) { m.AutoRoute(testGuildID, channel) }},
		{"member update", func(m *Service) {
			m.NotifyMemberUpdate(testGuildID, discord.Member{GuildID: testGuildID, User: discord.User{ID: caller}, RoleIDs: []snowflake.ID{roleTestCallerRoleID}})
		}},
	}
	for _, ep := range entryPoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newCallerRoleFixture(t)
			view := f.m.openGuildView(testGuildID)
			putInVoice(f, caller, channel, false, roleTestCallerRoleID)
			if view.HasListeners(channel) {
				t.Fatal("the view changed before anything refreshed it; it is not a snapshot")
			}

			ep.trigger(f.m)

			if got := view.EnumerateCallers(channel, roleTestCallerRoleID); !slices.Equal(got, []snowflake.ID{caller}) {
				t.Errorf("callers after %s = %v, want [%d]", ep.name, got, caller)
			}
		})
	}
}

func TestRefreshVoiceView_StopsWhenTheViewIsClosed(t *testing.T) {
	f := newCallerRoleFixture(t)
	channel := roleTestChannels[testGuildID][0]
	view := f.m.openGuildView(testGuildID)
	f.m.closeGuildView(testGuildID)
	putInVoice(f, 757, channel, false, roleTestCallerRoleID)

	f.m.RefreshVoiceView(testGuildID)

	if view.HasListeners(channel) {
		t.Error("a closed view kept being refreshed")
	}
}

func TestRaid_ViewClosedWhenTheRaidEnds(t *testing.T) {
	for _, side := range raidSides {
		t.Run(side.name+"/stopped", func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			cancel, err := side.start(f)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cancel()
			if _, open := f.m.views.Load(side.guildID); !open {
				t.Fatal("a running raid has no view of voice")
			}

			if err := f.m.StopVoiceRaid(context.Background(), side.guildID); err != nil {
				t.Fatalf("stop: %v", err)
			}

			if _, open := f.m.views.Load(side.guildID); open {
				t.Error("the view outlived the raid")
			}
		})
		t.Run(side.name+"/start failed after the view opened", func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			// The owner joins before the view opens; a session appearing by
			// then makes the commit at the very end of the start fail.
			f.ownerVM.onOpen = func() {
				f.m.mu.Lock()
				f.m.statuses[side.guildID].Session = &guild.Session{GuildID: side.guildID}
				f.m.mu.Unlock()
			}

			cancel, err := side.start(f)
			defer cancel()

			if err == nil {
				t.Fatal("start committed over an existing session")
			}
			if _, open := f.m.views.Load(side.guildID); open {
				t.Error("a failed start left its view of voice open")
			}
		})
	}
}
