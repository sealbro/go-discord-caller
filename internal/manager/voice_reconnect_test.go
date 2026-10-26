package manager

import (
	"testing"

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
