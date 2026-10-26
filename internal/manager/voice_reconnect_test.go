package manager

import (
	"slices"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// A reconnect promises a single retry after a short backoff. A failed join does
// not cost only the handshake timeout: pool.GuildVoice.Join releases the
// half-open conn afterwards, and that Leave blocks until disgo gives up on a
// conn Discord never answered for. With both attempts sharing one budget, that
// budget has to cover the whole sequence — otherwise the first failure leaves
// it already expired, the backoff select takes the cancelled branch, and the
// retry never runs.
func TestReconnectBudgetSurvivesAFailedJoin(t *testing.T) {
	t.Parallel()

	if reconnectBudget <= pool.VoiceJoinCost {
		t.Fatalf("reconnect budget %v is spent by one failed join (%v): the retry never runs", reconnectBudget, pool.VoiceJoinCost)
	}
	if want := 2*pool.VoiceJoinCost + reconnectBackoff; reconnectBudget < want {
		t.Errorf("reconnect budget = %v, want at least %v for two attempts plus the %v backoff", reconnectBudget, want, reconnectBackoff)
	}
}

// guildConn is shaped like disgo's connImpl, which is both the voice.Conn and
// the DAVE session's callbacks.
type guildConn struct {
	voice.Conn
	godave.Callbacks
	guildID snowflake.ID
}

func (c guildConn) GuildID() snowflake.ID { return c.guildID }

func TestEncryptStallResolvesToTheConnsGuildAndBot(t *testing.T) {
	t.Parallel()

	guildID, botUserID, ok := encryptStallTarget(godave.UserID(testBotID.String()), guildConn{guildID: testGuildID})
	if !ok {
		t.Fatal("a stall reported with a voice.Conn was not resolved")
	}
	if guildID != testGuildID || botUserID != testBotID {
		t.Errorf("resolved (%v, %v), want (%v, %v)", guildID, botUserID, testGuildID, testBotID)
	}
}

func TestEncryptStallWithoutAVoiceConnIsIgnored(t *testing.T) {
	t.Parallel()

	if _, _, ok := encryptStallTarget(godave.UserID(testBotID.String()), nil); ok {
		t.Error("a stall with no voice.Conn must not resolve to a rejoin")
	}
	if _, _, ok := encryptStallTarget("not-a-snowflake", guildConn{guildID: testGuildID}); ok {
		t.Error("a stall with an unparsable bot ID must not resolve to a rejoin")
	}
}

// A rejoin cannot repair a session that comes back just as broken: on
// 2026-10-26 two speakers whose DAVE session never became ready were rejoined
// every ~14 s for three hours. Stall reports for the same bot must back off.
func TestEncryptStallRejoinBacksOff(t *testing.T) {
	t.Parallel()

	m := newTestService(true)
	rejoins := 0
	m.rejoinStalled = func(guildID, botUserID snowflake.ID) { rejoins++ }

	conn := guildConn{guildID: testGuildID}
	for range 10 {
		m.OnEncryptStall(godave.UserID(testBotID.String()), conn)
	}

	if rejoins != 1 {
		t.Errorf("10 back-to-back stall reports caused %d rejoins, want 1", rejoins)
	}
}

func TestStallBackoffDoublesUpToTheCap(t *testing.T) {
	t.Parallel()

	b := newStallBackoff()
	k := botKey{testGuildID, testBotID}
	start := time.Unix(0, 0)

	var rejoinsAt []time.Duration
	for elapsed := time.Duration(0); elapsed <= 20*time.Minute; elapsed += 10 * time.Second {
		if b.due(k, start.Add(elapsed)) {
			rejoinsAt = append(rejoinsAt, elapsed)
		}
	}

	want := []time.Duration{0, 30 * time.Second, 90 * time.Second, 210 * time.Second, 450 * time.Second, 750 * time.Second, 1050 * time.Second}
	if !slices.Equal(rejoinsAt, want) {
		t.Errorf("rejoins at %v, want %v", rejoinsAt, want)
	}
}

func TestStallBackoffStartsOverAfterAQuietPeriod(t *testing.T) {
	t.Parallel()

	b := newStallBackoff()
	k := botKey{testGuildID, testBotID}
	start := time.Unix(0, 0)

	b.due(k, start)
	if b.due(k, start.Add(10*time.Second)) {
		t.Fatal("a second stall 10 s after a rejoin must back off")
	}
	if !b.due(k, start.Add(10*time.Second+stallRejoinBackoffMax)) {
		t.Error("a stall after a quiet period must rejoin immediately")
	}
}

func TestStallBackoffIsPerBot(t *testing.T) {
	t.Parallel()

	b := newStallBackoff()
	now := time.Unix(0, 0)
	b.due(botKey{testGuildID, testBotID}, now)
	if !b.due(botKey{testGuildID, testBotID + 1}, now) {
		t.Error("one bot's backoff delayed another bot's rejoin")
	}
}

func TestSessionTeardownForgetsStallBackoff(t *testing.T) {
	t.Parallel()

	rs := newReconnectState()
	now := time.Unix(0, 0)
	rs.stallRejoinDue(testGuildID, testBotID, now)
	rs.clearGuild(testGuildID, []snowflake.ID{testBotID}, testBotID+1)
	if !rs.stallRejoinDue(testGuildID, testBotID, now.Add(time.Second)) {
		t.Error("a stall in the next raid was held back by the previous raid's backoff")
	}
}
