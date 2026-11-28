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
// openView opens guildID's view of voice for a test that drives it by hand.
func openView(t *testing.T, m *Service, guildID snowflake.ID) *viewProbe {
	t.Helper()
	p, err := m.openGuildView(guildID)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	return p
}

func putInVoice(f *callerRoleFixture, userID, channelID snowflake.ID, bot bool, roles ...snowflake.ID) {
	putInGuildVoice(f, testGuildID, userID, channelID, bot, roles...)
}

func putInGuildVoice(f *callerRoleFixture, guildID, userID, channelID snowflake.ID, bot bool, roles ...snowflake.ID) {
	if roles != nil {
		f.caches.AddMember(discord.Member{GuildID: guildID, User: discord.User{ID: userID, Bot: bot}, RoleIDs: roles})
	}
	f.caches.AddVoiceState(discord.VoiceState{GuildID: guildID, ChannelID: &channelID, UserID: userID})
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

		view := openView(t, f.m, testGuildID)
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
		{"member update", func(m *Service) {
			m.NotifyMemberUpdate(testGuildID, discord.Member{GuildID: testGuildID, User: discord.User{ID: caller}, RoleIDs: []snowflake.ID{roleTestCallerRoleID}})
		}},
	}
	for _, ep := range entryPoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newCallerRoleFixture(t)
			view := openView(t, f.m, testGuildID)
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
	view := openView(t, f.m, testGuildID)
	f.m.closeGuildView(testGuildID, view)
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
		t.Run(side.name+"/stopped while the speakers join", func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The speakers join after the view opens; a /stop then fails the start.
			f.speakerVM.onOpen = cancel

			var err error
			if side.guildID == testGuildID {
				_, err = f.m.StartVoiceRaid(ctx, side.guildID, cancel, guild.RaidModeOneCaller)
			} else {
				_, err = f.m.JoinSession(ctx, side.guildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)
			}

			if err == nil {
				t.Fatal("a start stopped during the speaker joins succeeded")
			}
			if _, open := f.m.views.Load(side.guildID); open {
				t.Error("a failed start left its view of voice open")
			}
		})
	}
}

// A start that reaches the manager while a raid is already running is
// refused, and must leave the running raid's view of voice alone: otherwise
// that raid's capture filter, router and deaf state stop following voice for
// the rest of the raid.
func TestRaid_RefusedSecondStartLeavesTheRunningViewAlone(t *testing.T) {
	for _, side := range raidSides {
		t.Run(side.name, func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			channel := roleTestChannels[side.guildID][0]
			const userID = snowflake.ID(769)
			putInGuildVoice(f, side.guildID, userID, channel, false, viewTestOtherRole)
			cancel, err := side.start(f)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cancel()
			defer func() { _ = f.m.StopVoiceRaid(context.Background(), side.guildID) }()

			cancel2, err := side.start(f)
			cancel2()
			if err == nil {
				t.Fatal("a second start while the raid runs succeeded")
			}

			granted := discord.Member{GuildID: side.guildID, User: discord.User{ID: userID}, RoleIDs: []snowflake.ID{roleTestCallerRoleID}}
			f.caches.AddMember(granted)
			f.m.NotifyMemberUpdate(side.guildID, granted)

			f.m.mu.RLock()
			filter, _ := f.m.statuses[side.guildID].Session.AllowFilter.(*AllowFilter)
			f.m.mu.RUnlock()
			if filter == nil || !filter.Check(userID) {
				t.Error("after a refused second start, a caller role granted mid-raid is not relayed: the running raid's view stopped following voice")
			}
		})
	}
}

// A closed view stays closed only for the raid that owned it: a late close
// from an earlier raid's rollback must not take the view of the raid that
// replaced it.
func TestCloseGuildView_LeavesAnotherRaidsView(t *testing.T) {
	f := newCallerRoleFixture(t)
	earlier := openView(t, f.m, testGuildID)
	f.m.closeGuildView(testGuildID, earlier)
	current := openView(t, f.m, testGuildID)

	f.m.closeGuildView(testGuildID, earlier)

	if v, _ := f.m.views.Load(testGuildID); v != current {
		t.Error("closing an earlier raid's view closed the current raid's")
	}
}

// Members fetched at raid start change the view once, however many there are.
func TestMembersFetched_RefreshesTheViewOnce(t *testing.T) {
	f := newCallerRoleFixture(t)
	channel := roleTestChannels[testGuildID][0]
	view := openView(t, f.m, testGuildID)
	var members []discord.Member
	for id := snowflake.ID(770); id < 775; id++ {
		putInVoice(f, id, channel, false, roleTestCallerRoleID)
		members = append(members, discord.Member{User: discord.User{ID: id}})
	}
	refreshes := 0
	count := func(*viewProbe) { refreshes++ }
	view.watcher.Store(&count)

	f.m.membersFetched(testGuildID, members)

	if refreshes != 1 {
		t.Errorf("%d members fetched refreshed the view %d times, want once", len(members), refreshes)
	}
	if got := view.EnumerateCallers(channel, roleTestCallerRoleID); len(got) != len(members) {
		t.Errorf("callers after the fetch = %v, want all %d fetched members", got, len(members))
	}
}
