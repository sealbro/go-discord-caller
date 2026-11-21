package manager

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// productionIdleTimeout is the SESSION_IDLE_TIMEOUT default.
const productionIdleTimeout = 10 * time.Minute

// occupancy is a voice channel's occupants as the idle watcher can see them:
// who holds the caller role, which of those are not muted, and every human.
type occupancy struct {
	callers []snowflake.ID
	unmuted []snowflake.ID
	humans  []snowflake.ID
}

func (f *occupancy) EnumerateCallers(_, _ snowflake.ID) []snowflake.ID        { return f.callers }
func (f *occupancy) EnumerateUnmutedCallers(_, _ snowflake.ID) []snowflake.ID { return f.unmuted }
func (f *occupancy) Listeners(snowflake.ID) []snowflake.ID                    { return f.humans }

func TestSessionIdle(t *testing.T) {
	cases := []struct {
		name        string
		probe       occupancy
		sharesRelay bool
		wantStop    bool
		why         string
	}{
		{
			name:     "only non-callers in the channel",
			probe:    occupancy{humans: []snowflake.ID{2, 3}},
			wantStop: true,
			why:      "listeners without a caller must not keep the raid alive",
		},
		{
			name:     "a caller with a listener",
			probe:    occupancy{callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1, 2}},
			wantStop: false,
			why:      "a caller with someone to hear them must keep the raid alive",
		},
		// A caller with nobody else in the session's channels is heard by no
		// one, yet the idle timer only asked whether a caller was present, so
		// such a raid ran for as long as the caller stayed.
		{
			name:     "the only caller has nobody to hear them",
			probe:    occupancy{callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
			wantStop: true,
			why:      "a caller alone in the session's channels must not keep the raid alive",
		},
		// A muted caller sends nothing, so a channel full of them captures
		// nothing; the timer ignored mute state and kept such a raid alive
		// indefinitely.
		{
			name:     "every caller is muted",
			probe:    occupancy{callers: []snowflake.ID{1, 2}, humans: []snowflake.ID{1, 2, 3}},
			wantStop: true,
			why:      "callers who are all muted must not keep the raid alive",
		},
		{
			name:     "one caller is unmuted",
			probe:    occupancy{callers: []snowflake.ID{1, 2}, unmuted: []snowflake.ID{2}, humans: []snowflake.ID{1, 2}},
			wantStop: false,
			why:      "one unmuted caller, with the muted one to hear them, must keep the raid alive",
		},
		// With another server attached its members are out of sight, so the
		// session is judged as before: any caller in this server keeps it
		// alive, muted or alone.
		{
			name:        "a shared relay keeps a muted, lone caller's session",
			probe:       occupancy{callers: []snowflake.ID{1}, humans: []snowflake.ID{1}},
			sharesRelay: true,
			wantStop:    false,
			why:         "a muted caller must keep a session with relay peers alive",
		},
		{
			name:        "a shared relay without a caller",
			probe:       occupancy{humans: []snowflake.ID{2, 3}},
			sharesRelay: true,
			wantStop:    true,
			why:         "a session with relay peers but no caller must still be stopped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var stoppedAfter time.Duration
				w := &sessionPresenceWatcher{
					guildID:     testGuildID,
					channels:    []snowflake.ID{900},
					probe:       &tc.probe,
					callerRole:  func() snowflake.ID { return 1 },
					stop:        func() { stoppedAfter = time.Since(start) },
					idleTimeout: productionIdleTimeout,
					sharesRelay: func() bool { return tc.sharesRelay },
				}

				ctx, cancel := context.WithTimeout(t.Context(), 6*productionIdleTimeout)
				defer cancel()
				w.Run(ctx)

				switch {
				case tc.wantStop && stoppedAfter == 0:
					t.Error(tc.why)
				case !tc.wantStop && stoppedAfter != 0:
					t.Errorf("stopped after %v: %s", stoppedAfter, tc.why)
				case tc.wantStop && stoppedAfter != sessionPresenceInterval+productionIdleTimeout:
					// The first idle poll starts the clock; the stop comes on
					// the first poll a full timeout after it.
					t.Errorf("stopped after %v, want %v", stoppedAfter, sessionPresenceInterval+productionIdleTimeout)
				}
			})
		})
	}
}

// The watcher's mute rule is only as good as the probe's: a member who is
// muted by themselves or by the server must drop out of the unmuted callers
// while staying a caller and a listener.
func TestCacheVoiceProbe_UnmutedCallersSkipMutedMembers(t *testing.T) {
	t.Parallel()
	h := newBotOnlineHarness(t)
	const channel, role = snowflake.ID(900), snowflake.ID(1)
	caches := h.client.Caches

	for _, c := range []struct {
		id                snowflake.ID
		selfMute, srvMute bool
	}{{11, false, false}, {12, true, false}, {13, false, true}} {
		caches.AddMember(discord.Member{GuildID: testGuildID, User: discord.User{ID: c.id}, RoleIDs: []snowflake.ID{role}})
		ch := channel
		caches.AddVoiceState(discord.VoiceState{GuildID: testGuildID, ChannelID: &ch, UserID: c.id, SelfMute: c.selfMute, GuildMute: c.srvMute})
	}

	probe := &cacheVoiceProbe{svc: h.svc, guildID: testGuildID}
	if got := probe.EnumerateCallers(channel, role); len(got) != 3 {
		t.Errorf("want all 3 callers, muted or not, got %v", got)
	}
	if got := probe.EnumerateUnmutedCallers(channel, role); len(got) != 1 || got[0] != 11 {
		t.Errorf("want only the unmuted caller 11, got %v", got)
	}
	if got := probe.Listeners(channel); len(got) != 3 {
		t.Errorf("muted members still count as listeners, want 3, got %v", got)
	}
}

// A session stopped by the idle watcher must leave the owner's voice conn, as
// /stop does — cancelling the context alone closes the provider but leaves the
// audio sender running, polling it 50×/s for the life of the process.
// Production 2026-09-26T22:44:01Z: 1.41M errors over the following 8 h.
func TestIdleStop_TearsDownOwnerVoice(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newCallerRoleFixture(t)
		f.m.sessionIdleTimeout = productionIdleTimeout
		session := &guild.Session{GuildID: testGuildID, Cancel: func() {}, Cleanup: func() {}}
		f.m.statuses[testGuildID].Session = session

		// The cache holds no voice states, so the owner channel reads as
		// unoccupied from the first poll.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.m.startSessionIdleWatcher(ctx, cancel, session)

		time.Sleep(sessionPresenceInterval + productionIdleTimeout - time.Nanosecond)
		synctest.Wait()
		if n := f.ownerConn.closeCount(); n != 0 {
			t.Fatalf("owner left voice %d time(s) before the session was idle for the timeout", n)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if f.ownerConn.closeCount() == 0 {
			t.Error("idle stop must tear down the owner voice conn, but it never left the channel")
		}
		f.m.mu.RLock()
		defer f.m.mu.RUnlock()
		if s := f.m.statuses[testGuildID].Session; s != nil {
			t.Error("idle stop must clear the session")
		}
	})
}

// The watcher must only stop the session it was started for: the watched one can
// end and a fresh raid take its place before the stop runs, and stopping by guild
// alone would tear that new raid down seconds after it came up.
func TestIdleStop_LeavesANewerSessionAlone(t *testing.T) {
	t.Parallel()
	f := newCallerRoleFixture(t)
	watched := &guild.Session{GuildID: testGuildID, Cancel: func() {}, Cleanup: func() {}}
	newer := &guild.Session{GuildID: testGuildID, Cancel: func() {}, Cleanup: func() {}}

	// The watched session ends and a new raid takes its place before the
	// watcher's stop runs.
	f.m.statuses[testGuildID].Session = newer

	f.m.stopIdleSession(watched, func() {})

	f.m.mu.RLock()
	defer f.m.mu.RUnlock()
	if got := f.m.statuses[testGuildID].Session; got != newer {
		t.Error("idle stop tore down a session it was not watching")
	}
	if n := f.ownerConn.closeCount(); n != 0 {
		t.Error("idle stop left the newer session's voice conn")
	}
}
