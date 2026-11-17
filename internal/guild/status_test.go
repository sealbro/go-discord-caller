package guild

import (
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

const (
	testGuild   snowflake.ID = 1
	testOwner   snowflake.ID = 2
	testSpeakA  snowflake.ID = 10
	testSpeakB  snowflake.ID = 20
	testChannel snowflake.ID = 100
	testRole    snowflake.ID = 1000
)

func TestGetSortedSpeakersOrdersByUsername(t *testing.T) {
	st := NewStatus(testGuild, testOwner)
	st.Speakers[testSpeakA] = &Speaker{ID: testSpeakA, Username: "zeta"}
	st.Speakers[testSpeakB] = &Speaker{ID: testSpeakB, Username: "alpha"}

	got := st.GetSortedSpeakers()
	if len(got) != 2 || got[0].Username != "alpha" || got[1].Username != "zeta" {
		t.Fatalf("GetSortedSpeakers = %+v, want alpha then zeta", got)
	}
}

func TestRenderEnglishFallback(t *testing.T) {
	role := testRole
	unconfigured := NewStatus(testGuild, testOwner)

	configured := NewStatus(testGuild, testOwner)
	configured.GuildName = "Raid Guild"
	configured.CallerRoleID = &role
	configured.ManagerRoleID = &role
	configured.AllyCode = "ABCDEFGH"
	configured.BoundChannels[testOwner] = testChannel
	configured.BoundChannels[testSpeakA] = testChannel
	configured.Speakers[testSpeakA] = &Speaker{ID: testSpeakA, Username: "a", Enabled: true}
	configured.Speakers[testSpeakB] = &Speaker{ID: testSpeakB, Username: "b"}

	withSession := func(s *Session, mutate func(*Status)) *Status {
		st := NewStatus(testGuild, testOwner)
		st.Session = s
		if mutate != nil {
			mutate(st)
		}
		return st
	}
	twoSpeakers := []Speaker{{ID: testSpeakA}, {ID: testSpeakB}}

	tests := []struct {
		name string
		st   *Status
		want []string
	}{
		{
			name: "unconfigured",
			st:   unconfigured,
			want: []string{
				"**Capture Role:** ⚠️ not set — required: voice raids cannot start",
				"**Manager Role:** not set",
				"**Bots (1):** ⚠️ 1 unbound",
				"- 🎙️ <@2> → unbound",
				"**Voice Raid:** ⚫ inactive",
			},
		},
		{
			name: "configured",
			st:   configured,
			want: []string{
				"**Guild:** Raid Guild",
				"**Capture Role:** <@&1000>",
				"**Manager Role:** <@&1000>",
				"**Bots (3):** ✅ 2 ready · 🔇 1 disabled",
				"- 🎙️ <@2> → <#100>",
				"- 🔊 <@10> → <#100>",
				"- 🔇 <@20> → unbound",
				"**Ally Code:** `ABCDEFGH`",
			},
		},
		{
			name: "host without guests",
			st:   withSession(&Session{Speakers: twoSpeakers}, nil),
			want: []string{"**Voice Raid:** 🔴 active (2 speakers joined)"},
		},
		{
			name: "host with guests",
			st: withSession(&Session{Speakers: twoSpeakers}, func(s *Status) {
				s.GuestGuildNames = []string{"One", "Two"}
			}),
			want: []string{"**Voice Raid:** 🔴 active (2 speakers joined) — guests: One, Two"},
		},
		{
			name: "guest with unknown host name",
			st:   withSession(&Session{Speakers: twoSpeakers, IsGuest: true, AllyCode: "CODE"}, nil),
			want: []string{"**Voice Raid:** 🔴 guest relay → CODE (2 speakers joined)"},
		},
		{
			name: "guest with host name",
			st: withSession(&Session{Speakers: twoSpeakers, IsGuest: true, AllyCode: "CODE"}, func(s *Status) {
				s.HostGuildName = "Host"
			}),
			want: []string{"**Voice Raid:** 🔴 guest relay → Host (`CODE`) (2 speakers joined)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.st.Render(nil)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("Render missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

func TestEnglishStatusFallbackUnknownKey(t *testing.T) {
	if got := englishStatusFallback("status.nope"); got != "status.nope" {
		t.Fatalf("englishStatusFallback(unknown) = %q, want the key", got)
	}
}

type recordingTranslator struct{ keys []string }

func (r *recordingTranslator) T(key string, _ ...any) string {
	r.keys = append(r.keys, key)
	return key
}

func TestRenderUsesTranslator(t *testing.T) {
	tr := &recordingTranslator{}
	NewStatus(testGuild, testOwner).Render(tr)
	if len(tr.keys) == 0 {
		t.Fatal("Render did not consult the translator")
	}
}

func TestHasActiveSession(t *testing.T) {
	t.Parallel()
	s := NewStatus(testGuild, testOwner)
	if s.HasActiveSession() {
		t.Error("fresh status must have no active session")
	}
	s.Session = &Session{GuildID: testGuild}
	if !s.HasActiveSession() {
		t.Error("status with a session must report it active")
	}
}

func TestRenderEscapesOtherGuildsNames(t *testing.T) {
	const hostile = "[Official support](https://evil.example) **now**"
	const escaped = `\[Official support\]\(https://evil.example\) \*\*now\*\*`

	t.Run("own guild", func(t *testing.T) {
		st := NewStatus(testGuild, testOwner)
		st.GuildName = hostile
		assertEscaped(t, st.Render(nil), hostile, escaped)
	})
	t.Run("host seen by a guest", func(t *testing.T) {
		st := NewStatus(testGuild, testOwner)
		st.Session = &Session{IsGuest: true, AllyCode: "ABCD2345"}
		st.HostGuildName = hostile
		assertEscaped(t, st.Render(nil), hostile, escaped)
	})
	t.Run("guests seen by the host", func(t *testing.T) {
		st := NewStatus(testGuild, testOwner)
		st.Session = &Session{}
		st.GuestGuildNames = []string{hostile, "plain"}
		assertEscaped(t, st.Render(nil), hostile, escaped)
	})
}

func assertEscaped(t *testing.T, out, raw, escaped string) {
	t.Helper()
	if strings.Contains(out, raw) {
		t.Errorf("a guild name reached the message as live markdown:\n%s", out)
	}
	if !strings.Contains(out, escaped) {
		t.Errorf("escaped name %q missing from:\n%s", escaped, out)
	}
}

func TestSpeakerCountsSumToAllSpeakers(t *testing.T) {
	const (
		readyID         snowflake.ID = 11
		unboundID       snowflake.ID = 12
		disabledID      snowflake.ID = 13
		disabledUnbound snowflake.ID = 14
	)
	st := NewStatus(testGuild, testOwner)
	st.Speakers[readyID] = &Speaker{ID: readyID, Enabled: true}
	st.Speakers[unboundID] = &Speaker{ID: unboundID, Enabled: true}
	st.Speakers[disabledID] = &Speaker{ID: disabledID}
	st.Speakers[disabledUnbound] = &Speaker{ID: disabledUnbound}
	st.BoundChannels[readyID] = testChannel
	st.BoundChannels[disabledID] = testChannel

	ready, disabled, unbound := st.SpeakerCounts()
	if ready != 1 || disabled != 2 || unbound != 1 {
		t.Errorf("SpeakerCounts = ready %d, disabled %d, unbound %d; want 1, 2, 1", ready, disabled, unbound)
	}
	if got := ready + disabled + unbound; got != len(st.Speakers) {
		t.Errorf("counts sum to %d, want %d speakers", got, len(st.Speakers))
	}
}

func TestRenderOmitsBreakdownWhenEveryBotIsReady(t *testing.T) {
	st := NewStatus(testGuild, testOwner)
	st.Speakers[testSpeakA] = &Speaker{ID: testSpeakA, Username: "a", Enabled: true}
	st.Speakers[testSpeakB] = &Speaker{ID: testSpeakB, Username: "b", Enabled: true}
	st.BoundChannels[testOwner] = testChannel
	st.BoundChannels[testSpeakA] = testChannel
	st.BoundChannels[testSpeakB] = testChannel

	got := st.Render(nil)
	if !strings.Contains(got, "**Bots (3):**\n") {
		t.Errorf("header should carry no breakdown when all speakers are ready:\n%s", got)
	}
	if strings.Contains(got, "ready") {
		t.Errorf("a status with every speaker ready should not repeat the count:\n%s", got)
	}
}

func TestRenderKeepsConfigFieldsOnConsecutiveLinesAndListsOwner(t *testing.T) {
	role := testRole
	st := NewStatus(testGuild, testOwner)
	st.GuildName = "Raid Guild"
	st.CallerRoleID = &role
	st.ManagerRoleID = &role
	st.BoundChannels[testOwner] = testChannel

	want := "**Guild:** Raid Guild\n" +
		"**Capture Role:** <@&1000>\n" +
		"**Manager Role:** <@&1000>\n\n" +
		"**Bots (1):**\n" +
		"- 🎙️ <@2> → <#100>\n"
	if got := st.Render(nil); !strings.HasPrefix(got, want) {
		t.Errorf("config fields should sit on consecutive lines:\n%s", got)
	}
}
