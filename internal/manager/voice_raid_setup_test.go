package manager

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// A speaker only becomes a join candidate when it is BOTH enabled and bound to
// a channel in this guild. An empty result is what separates ErrNoBoundSpeakers
// ("never set up") from ErrNoSpeakers ("set up, but nothing connected").
func TestBoundSpeakers(t *testing.T) {
	t.Parallel()

	const guildID = snowflake.ID(100)
	var (
		enabledBound   = snowflake.ID(1)
		enabledUnbound = snowflake.ID(2)
		disabledBound  = snowflake.ID(3)
		otherGuildOnly = snowflake.ID(4)
	)

	st := store.NewInMemoryStore()
	st.BindChannel(guildID, enabledBound, snowflake.ID(900))
	st.BindChannel(guildID, disabledBound, snowflake.ID(901))
	st.BindChannel(snowflake.ID(200), otherGuildOnly, snowflake.ID(902))

	speakers := []guild.Speaker{
		{ID: enabledBound, Enabled: true},
		{ID: enabledUnbound, Enabled: true},
		{ID: disabledBound, Enabled: false},
		{ID: otherGuildOnly, Enabled: true},
	}

	got := boundSpeakers(st, guildID, speakers)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].ID != enabledBound {
		t.Errorf("want speaker %s, got %s", enabledBound, got[0].ID)
	}
}

// A guild that was never configured yields no candidates at all — the guest
// guild case from issue #51's follow-up report.
func TestBoundSpeakers_UnconfiguredGuild(t *testing.T) {
	t.Parallel()

	speakers := []guild.Speaker{
		{ID: snowflake.ID(1), Enabled: true},
		{ID: snowflake.ID(2), Enabled: true},
	}
	if got := boundSpeakers(store.NewInMemoryStore(), snowflake.ID(100), speakers); len(got) != 0 {
		t.Errorf("unbound guild must produce no candidates, got %+v", got)
	}
}
