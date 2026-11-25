package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Without a caller role every non-bot user in the captured channel is relayed —
// to every speaker channel and, through a relay code, into other servers. A
// raid must therefore not start until the role is bound, and must refuse before
// any bot joins voice.
func TestStartVoiceRaidRefusedWithoutCallerRole(t *testing.T) {
	for _, mode := range []guild.RaidMode{guild.RaidModeOneCaller, guild.RaidModeGuildCaller, guild.RaidModeOneManyGuildCaller} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCallerRoleFixture(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, mode)

			if !errors.Is(err, ErrNoCallerRole) {
				t.Errorf("StartVoiceRaid without a caller role: err = %v, want ErrNoCallerRole", err)
			}
			if n := f.joins.Load(); n != 0 {
				t.Errorf("%d bot(s) joined voice for a raid that must be refused", n)
			}
			if f.m.HasActiveSession(testGuildID) {
				t.Error("a raid without a caller role was committed")
			}
		})
	}
}

// A guest that captures relays its own users into the host's server, so it
// needs a caller role just as a host does. The check applies to the effective
// mode: the guest is refused before it joins voice and leaves the ally session.
func TestJoinSessionRefusedWithoutCallerRoleWhenGuestCaptures(t *testing.T) {
	for _, mode := range []guild.RaidMode{guild.RaidModeAllyCaller, guild.RaidModeOneManyAllyCaller} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCallerRoleFixture(t)
			f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, mode, roleTestHostCode)

			if !errors.Is(err, ErrNoCallerRole) {
				t.Errorf("capturing guest without a caller role: err = %v, want ErrNoCallerRole", err)
			}
			if n := f.joins.Load(); n != 0 {
				t.Errorf("%d bot(s) joined voice for a guest join that must be refused", n)
			}
			if _, attached := f.m.sessions.GetByGuild(roleTestGuestGuildID); attached {
				t.Error("refused guest is still attached to the host's ally session")
			}
		})
	}
}

// Guard against over-blocking: a guest that only listens relays nobody, so it
// needs no caller role — including a capturing mode the host downgrades.
func TestJoinSessionAllowsListenerWithoutCallerRole(t *testing.T) {
	cases := []struct {
		name      string
		hostMode  guild.RaidMode
		guestMode guild.RaidMode
	}{
		{"listener", guild.RaidModeGuildCaller, guild.RaidModeAllyListener},
		{"downgraded by host", guild.RaidModeOneCaller, guild.RaidModeAllyCaller},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCallerRoleFixture(t)
			f.m.sessions.Create(roleTestHostCode, testGuildID, tc.hostMode)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			effective, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, tc.guestMode, roleTestHostCode)
			if err != nil {
				t.Fatalf("listen-only guest without a caller role: err = %v, want nil", err)
			}
			if effective.WithCapture() {
				t.Fatalf("effective guest mode %s captures; the case is meant to be listen-only", effective)
			}
		})
	}
}

// HasCallerRole decides who the voice-callers metric counts and, through
// member updates, who the live allow filter relays. With no role bound it must
// match nobody, or unbinding the role mid-raid would relay every user.
func TestHasCallerRoleFalseWhenNoRoleBound(t *testing.T) {
	f := newCallerRoleFixture(t)

	if f.m.HasCallerRole(testGuildID, []snowflake.ID{roleTestCallerRoleID}) {
		t.Error("HasCallerRole with no caller role bound = true; an unbound role must make nobody a caller")
	}
}

func TestAllowFilterWithoutCallerRoleRelaysNobody(t *testing.T) {
	const userID = snowflake.ID(42)
	f := newCallerRoleFixture(t)
	putInVoice(f, userID, roleTestChannels[testGuildID][0], false, roleTestCallerRoleID)

	if f.m.buildAllowUserFilter(testGuildID, f.m.openGuildView(testGuildID)).Check(userID) {
		t.Error("allow filter with no caller role relays a member; it must relay nobody")
	}
}

// Guard: with a role bound, only its holders are relayed.
func TestAllowFilterRelaysOnlyCallerRoleHolders(t *testing.T) {
	const (
		callerID    = snowflake.ID(42)
		bystanderID = snowflake.ID(43)
	)
	f := newCallerRoleFixture(t)
	f.st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	putInVoice(f, callerID, roleTestChannels[testGuildID][0], false, roleTestCallerRoleID)
	putInVoice(f, bystanderID, roleTestChannels[testGuildID][0], false, viewTestOtherRole)

	filter := f.m.buildAllowUserFilter(testGuildID, f.m.openGuildView(testGuildID))
	if !filter.Check(callerID) {
		t.Error("member holding the caller role is not relayed")
	}
	if filter.Check(bystanderID) {
		t.Error("member without the caller role is relayed")
	}
}

// Speakers that must not hear are server-deafened by the owner bot, so a guild
// where it cannot do that is refused before any bot joins voice.
func TestRaidRefusedWithoutDeafenPermission(t *testing.T) {
	f := newCallerRoleFixture(t)
	f.caches.AddRole(discord.Role{ID: testGuildID, GuildID: testGuildID, Permissions: ChannelAccessPermissions})
	f.addMember(testGuildID, testBotID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)

	if !errors.Is(err, ErrNoDeafenPermission) {
		t.Errorf("StartVoiceRaid without Deafen Members: err = %v, want ErrNoDeafenPermission", err)
	}
	if n := f.joins.Load(); n != 0 {
		t.Errorf("%d bot(s) joined voice for a raid that must be refused", n)
	}

	f.m.sessions.Create(roleTestHostCode, roleTestGuestGuildID, guild.RaidModeGuildCaller)
	_, err = f.m.JoinSession(ctx, testGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)
	if !errors.Is(err, ErrNoDeafenPermission) {
		t.Errorf("JoinSession without Deafen Members: err = %v, want ErrNoDeafenPermission", err)
	}
}
