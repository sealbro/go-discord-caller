package manager

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
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

// voiceOccupancy is the watcher's probe over several channels.
type voiceOccupancy map[snowflake.ID]occupancy

func (v voiceOccupancy) EnumerateCallers(ch, _ snowflake.ID) []snowflake.ID { return v[ch].callers }
func (v voiceOccupancy) EnumerateUnmutedCallers(ch, _ snowflake.ID) []snowflake.ID {
	return v[ch].unmuted
}
func (v voiceOccupancy) Listeners(ch snowflake.ID) []snowflake.ID { return v[ch].humans }

// The owner bot captures in ownerChannel; the speakers play into speakerA and
// speakerB.
const (
	ownerChannel = snowflake.ID(900)
	speakerA     = snowflake.ID(901)
	speakerB     = snowflake.ID(902)
)

func TestSessionIdle(t *testing.T) {
	cases := []struct {
		name        string
		probe       voiceOccupancy
		silent      bool
		sharesRelay bool
		wantStop    bool
		why         string
	}{
		{
			name:     "only non-callers in the channels",
			probe:    voiceOccupancy{ownerChannel: {humans: []snowflake.ID{2}}, speakerA: {humans: []snowflake.ID{3}}},
			wantStop: true,
			why:      "listeners without a caller must not keep the raid alive",
		},
		{
			name: "a caller heard in a speaker channel",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
				speakerA:     {humans: []snowflake.ID{2}},
			},
			wantStop: false,
			why:      "a speaking caller with a listener in a speaker channel must keep the raid alive",
		},
		{
			name: "the only caller has nobody to hear them",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
			},
			wantStop: true,
			why:      "a caller alone in the session's channels must not keep the raid alive",
		},
		// The speakers play into their own channels; someone sitting next to
		// the caller hears them without the bot. Counting that person, or a
		// second caller, as the audience kept raids with empty speaker
		// channels alive for as long as the callers stayed.
		{
			name: "callers together with nobody in the speaker channels",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1, 2}, unmuted: []snowflake.ID{1, 2}, humans: []snowflake.ID{1, 2, 3}},
			},
			wantStop: true,
			why:      "people in the caller's own channel must not count as the speakers' audience",
		},
		// Discord sends nothing while a member is quiet, so callers who stay
		// in voice but never speak kept a raid alive with nothing to relay
		// (production 2026-11-22: 1.5 h without a single inbound frame).
		{
			name: "callers and listeners present but nobody speaks",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
				speakerA:     {humans: []snowflake.ID{2}},
			},
			silent:   true,
			wantStop: true,
			why:      "a raid where no caller speaks must not be kept alive by presence alone",
		},
		{
			name: "every caller is muted",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1, 2}, humans: []snowflake.ID{1, 2}},
				speakerA:     {humans: []snowflake.ID{3}},
			},
			wantStop: true,
			why:      "callers who are all muted must not keep the raid alive",
		},
		{
			name: "one caller is unmuted",
			probe: voiceOccupancy{
				ownerChannel: {callers: []snowflake.ID{1, 2}, unmuted: []snowflake.ID{2}, humans: []snowflake.ID{1, 2}},
				speakerA:     {humans: []snowflake.ID{3}},
			},
			wantStop: false,
			why:      "one unmuted caller with a listener in a speaker channel must keep the raid alive",
		},
		// With callers in the speaker channels themselves, each one hears the
		// other through their channel's speaker.
		{
			name: "callers in two speaker channels hear each other",
			probe: voiceOccupancy{
				speakerA: {callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
				speakerB: {callers: []snowflake.ID{2}, unmuted: []snowflake.ID{2}, humans: []snowflake.ID{2}},
			},
			wantStop: false,
			why:      "a caller in another speaker channel is an audience and must keep the raid alive",
		},
		// With another server attached its members are out of sight, so the
		// session is judged on callers alone: any caller in this server keeps
		// it alive, muted, alone or silent.
		{
			name:        "a shared relay keeps a muted, lone, silent caller's session",
			probe:       voiceOccupancy{ownerChannel: {callers: []snowflake.ID{1}, humans: []snowflake.ID{1}}},
			silent:      true,
			sharesRelay: true,
			wantStop:    false,
			why:         "a muted caller must keep a session with relay peers alive",
		},
		{
			name:        "a shared relay without a caller",
			probe:       voiceOccupancy{ownerChannel: {humans: []snowflake.ID{2, 3}}},
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
				w := newTestWatcher(tc.probe, func() { stoppedAfter = time.Since(start) })
				w.sharesRelay = func() bool { return tc.sharesRelay }
				if tc.silent {
					w.lastHeard = func() time.Time { return time.Time{} }
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

// newTestWatcher watches ownerChannel, speakerA and speakerB, with the speakers
// playing into the last two and a caller speaking on every poll.
func newTestWatcher(probe voiceOccupancy, stop func()) *sessionPresenceWatcher {
	return &sessionPresenceWatcher{
		guildID:          testGuildID,
		channels:         []snowflake.ID{ownerChannel, speakerA, speakerB},
		listenerChannels: []snowflake.ID{speakerA, speakerB},
		probe:            probe,
		callerRole:       func() snowflake.ID { return 1 },
		stop:             stop,
		idleTimeout:      productionIdleTimeout,
		lastHeard:        time.Now,
	}
}

// The idle clock starts once callers fall quiet, not at the session's start: a
// raid that talked for half an hour stops a timeout after its last words.
func TestSessionIdle_ClockStartsWhenCallersFallQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const talked = 30 * time.Minute
		start := time.Now()
		var stoppedAfter time.Duration
		w := newTestWatcher(voiceOccupancy{
			ownerChannel: {callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}},
			speakerA:     {humans: []snowflake.ID{2}},
		}, func() { stoppedAfter = time.Since(start) })
		quietFrom := start.Add(talked)
		w.lastHeard = func() time.Time {
			if now := time.Now(); now.Before(quietFrom) {
				return now
			}
			return quietFrom
		}

		ctx, cancel := context.WithTimeout(t.Context(), talked+6*productionIdleTimeout)
		defer cancel()
		w.Run(ctx)

		// The first poll with no frame in the last interval starts the clock.
		want := talked + sessionPresenceInterval + productionIdleTimeout
		if stoppedAfter != want {
			t.Errorf("stopped %v after the start, want %v: %v of talk, then the timeout from the first quiet poll", stoppedAfter, want, talked)
		}
	})
}

// The watcher's mute rule is only as good as the probe's: a member who is
// muted by themselves or by the server must drop out of the unmuted callers
// while staying a caller and a listener.
func TestCacheVoiceProbe_UnmutedCallersSkipMutedMembers(t *testing.T) {
	t.Parallel()
	h := newGuildMetricHarness(t)
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

// The watcher learns of speech from the session's allow filter, so every
// capturing receiver must record the frames it lets through there.
func TestStartVoiceRaid_CallerFramesCountAsSpeech(t *testing.T) {
	const callerID = snowflake.ID(42)
	f := withCallerRole(newCallerRoleFixture(t))
	f.addMember(testGuildID, callerID, roleTestCallerRoleID)

	_, cancel, err := startHostRaid(t, f)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cancel()
	defer func() { _ = f.m.StopVoiceRaid(context.Background(), testGuildID) }()

	f.m.mu.RLock()
	filter, _ := f.m.statuses[testGuildID].Session.AllowFilter.(*AllowFilter)
	f.m.mu.RUnlock()
	receiver := f.ownerConn.frameReceiver()
	if filter == nil || receiver == nil {
		t.Fatal("raid has no allow filter or no owner receiver")
	}

	if err := receiver.ReceiveOpusFrame(callerID, &voice.Packet{Opus: []byte{0xf8, 0xff, 0xfe}}); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if filter.LastAdmitted().IsZero() {
		t.Error("a caller's frame reached the owner's receiver but the session never saw it: the idle watcher would stop a raid that is talking")
	}
}

// Only the channels a bot plays into hold the speakers' audience. The owner's
// channel is one of them in capture modes, where the owner plays the other
// channels back to its own callers.
func TestSessionChannels_ListenerChannelsFollowPlayback(t *testing.T) {
	t.Parallel()
	ownerCh, speakerCh := roleTestChannels[testGuildID][0], roleTestChannels[testGuildID][1]
	for _, tc := range []struct {
		mode guild.RaidMode
		want []snowflake.ID
	}{
		{guild.RaidModeOneCaller, []snowflake.ID{speakerCh}},
		{guild.RaidModeGuildCaller, []snowflake.ID{ownerCh, speakerCh}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			f := newCallerRoleFixture(t)
			session := &guild.Session{GuildID: testGuildID, Mode: tc.mode, Speakers: []guild.Speaker{{ID: roleTestSpeakerID}}}

			channels, listeners := f.m.sessionChannels(session)
			if !slices.Equal(channels, []snowflake.ID{ownerCh, speakerCh}) {
				t.Errorf("watched channels = %v, want the owner's and the speaker's", channels)
			}
			if !slices.Equal(listeners, tc.want) {
				t.Errorf("listener channels = %v, want %v", listeners, tc.want)
			}
		})
	}
}
