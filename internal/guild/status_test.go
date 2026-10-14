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
				"**Capture Role:** not set",
				"**Manager Role:** not set",
				"**Owner Bot Channel:** not set",
				"**Speakers (0):**",
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
				"**Owner Bot Channel:** <#100>",
				"**Speakers (2):**",
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
