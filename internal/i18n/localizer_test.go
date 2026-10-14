package i18n

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
)

func TestForResolvesLocale(t *testing.T) {
	b := MustBundle()

	tests := []struct {
		name        string
		guild       string
		interaction discord.Locale
		want        string
	}{
		{"guild pin wins", "de", discord.LocaleRussian, "de"},
		{"interaction locale", "", discord.LocaleRussian, "ru"},
		{"unknown guild pin falls through", "xx", discord.LocalePolish, "pl"},
		{"unmapped interaction locale", "", discord.LocaleJapanese, DefaultLocale},
		{"nothing set", "", "", DefaultLocale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := b.For(tt.guild, tt.interaction).Locale(); got != tt.want {
				t.Fatalf("For(%q, %q).Locale() = %q, want %q", tt.guild, tt.interaction, got, tt.want)
			}
		})
	}
}

func TestLocalizerT(t *testing.T) {
	loc := MustBundle().For("en", "")

	tests := []struct {
		name string
		key  string
		args []any
		want string
	}{
		{"plain", "err.guild_only", nil, "this command can only be used inside a server"},
		{"plural with template data", "status.raid_guest", []any{"Count", 1, "Host", "H"}, "🔴 guest relay → H (1 speaker joined)"},
		{"non-string var name skipped", "status.raid_guest", []any{42, "x", "Count", 2, "Host", "H"}, "🔴 guest relay → H (2 speakers joined)"},
		{"missing key returns key", "no.such.key", nil, "no.such.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loc.T(tt.key, tt.args...); got != tt.want {
				t.Fatalf("T(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestDisplayNameCoversSupportedLocales(t *testing.T) {
	seen := make(map[string]string)
	for _, l := range SupportedBundleLocales() {
		name := DisplayName(l)
		if name == "" || name == l {
			t.Errorf("DisplayName(%q) = %q, want a native-language name", l, name)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("DisplayName(%q) duplicates DisplayName(%q) = %q", l, prev, name)
		}
		seen[name] = l
	}
	if got := DisplayName("xx"); got != "xx" {
		t.Errorf("DisplayName(unknown) = %q, want the code itself", got)
	}
}
