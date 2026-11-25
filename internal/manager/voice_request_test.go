package manager

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// A stall report names a guild and a bot; only a bot that is actually part of
// that guild's raid may be rejoined, so a report cannot move bots of another
// raid or of none.
func TestHandleVoiceRequest_EncryptStallOnlyForBotsInTheRaid(t *testing.T) {
	const speakerID, strangerID = snowflake.ID(764), snowflake.ID(765)
	cases := []struct {
		name    string
		session bool
		botID   snowflake.ID
		want    bool
	}{
		{"owner of the raid", true, testBotID, true},
		{"speaker of the raid", true, speakerID, true},
		{"bot outside the raid", true, strangerID, false},
		{"guild without a raid", false, testBotID, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestService(c.session)
			m.ownerBotID = testBotID
			if c.session {
				m.statuses[testGuildID].Session.Speakers = []guild.Speaker{{ID: speakerID, Enabled: true}}
			}
			var rejoined []snowflake.ID
			m.rejoinStalled = func(_, botID snowflake.ID) { rejoined = append(rejoined, botID) }

			got := m.handleVoiceRequest(encryptStallRequest{GuildID: testGuildID, BotID: c.botID})

			if got != c.want || (len(rejoined) == 1) != c.want {
				t.Errorf("handled = %v, rejoined %v; want handled and rejoined = %v", got, rejoined, c.want)
			}
		})
	}
}
