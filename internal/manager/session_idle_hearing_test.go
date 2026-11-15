package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// hearingOccupancy is a voice channel's occupants as the idle watcher can see
// them: who holds the caller role, which of those are not muted, and every human.
type hearingOccupancy struct {
	callers []snowflake.ID
	unmuted []snowflake.ID
	humans  []snowflake.ID
}

func (f *hearingOccupancy) EnumerateCallers(_, _ snowflake.ID) []snowflake.ID { return f.callers }

func (f *hearingOccupancy) EnumerateUnmutedCallers(_, _ snowflake.ID) []snowflake.ID {
	return f.unmuted
}

func (f *hearingOccupancy) Listeners(snowflake.ID) []snowflake.ID { return f.humans }

func stoppedWithin(w *sessionPresenceWatcher, d time.Duration) bool {
	var stopped atomic.Bool
	w.stop = func() { stopped.Store(true) }
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	w.Run(ctx)
	return stopped.Load()
}

// A caller with nobody else in the session's channels is heard by no one, yet
// the idle timer only asked whether a caller was present, so such a raid ran for
// as long as the caller stayed.
func TestSessionIdle_StopsWhenTheOnlyCallerHasNobodyToHear(t *testing.T) {
	probe := &hearingOccupancy{callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1}}

	if !stoppedWithin(newIdleWatcher(7700010, probe), 200*time.Millisecond) {
		t.Error("a caller alone in the session's channels must not keep the raid alive")
	}
}

func TestSessionIdle_KeptAliveWhileACallerHasAListener(t *testing.T) {
	probe := &hearingOccupancy{callers: []snowflake.ID{1}, unmuted: []snowflake.ID{1}, humans: []snowflake.ID{1, 2}}

	if stoppedWithin(newIdleWatcher(7700011, probe), 200*time.Millisecond) {
		t.Error("a caller with someone to hear them must keep the raid alive")
	}
}

// A muted caller sends nothing, so a channel full of them captures nothing; the
// timer ignored mute state and kept such a raid alive indefinitely.
func TestSessionIdle_StopsWhenEveryCallerIsMuted(t *testing.T) {
	probe := &hearingOccupancy{callers: []snowflake.ID{1, 2}, humans: []snowflake.ID{1, 2, 3}}

	if !stoppedWithin(newIdleWatcher(7700012, probe), 200*time.Millisecond) {
		t.Error("callers who are all muted must not keep the raid alive")
	}
}

func TestSessionIdle_KeptAliveWhileOneCallerIsUnmuted(t *testing.T) {
	probe := &hearingOccupancy{callers: []snowflake.ID{1, 2}, unmuted: []snowflake.ID{2}, humans: []snowflake.ID{1, 2}}

	if stoppedWithin(newIdleWatcher(7700013, probe), 200*time.Millisecond) {
		t.Error("one unmuted caller, with the muted one to hear them, must keep the raid alive")
	}
}

// With another server attached its members are out of sight, so the session is
// judged as before: any caller in this server keeps it alive, muted or alone.
func TestSessionIdle_ASharedRelayKeepsTheCallerRule(t *testing.T) {
	probe := &hearingOccupancy{callers: []snowflake.ID{1}, humans: []snowflake.ID{1}}
	w := newIdleWatcher(7700014, probe)
	w.sharesRelay = func() bool { return true }

	if stoppedWithin(w, 200*time.Millisecond) {
		t.Error("a muted caller must keep a session with relay peers alive")
	}
}

func TestSessionIdle_ASharedRelayStillStopsWithoutACaller(t *testing.T) {
	probe := &hearingOccupancy{humans: []snowflake.ID{2, 3}}
	w := newIdleWatcher(7700015, probe)
	w.sharesRelay = func() bool { return true }

	if !stoppedWithin(w, 200*time.Millisecond) {
		t.Error("a session with relay peers but no caller must still be stopped")
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
