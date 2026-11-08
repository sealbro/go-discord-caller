package i18n

import "testing"

func TestLocalizationsSkipDefaultAndCoverOtherLocales(t *testing.T) {
	t.Parallel()
	b := MustBundle()

	got := b.DescriptionLocalizations("cmd.setup.description")
	if len(got) == 0 {
		t.Fatal("expected translations for the other bundle locales")
	}
	for _, d := range DiscordLocalesFor(DefaultLocale) {
		if _, ok := got[d]; ok {
			t.Errorf("default-locale entry %v must be left to the top-level field", d)
		}
	}
	for d, v := range got {
		if v == "" || v == "cmd.setup.description" {
			t.Errorf("locale %v has untranslated value %q", d, v)
		}
	}

	if names := b.NameLocalizations("cmd.setup.description"); len(names) != len(got) {
		t.Errorf("NameLocalizations has %d entries, want %d", len(names), len(got))
	}
}

func TestLocalizationsOfUnknownKeyAreEmpty(t *testing.T) {
	t.Parallel()
	if got := MustBundle().DescriptionLocalizations("no.such.key"); len(got) != 0 {
		t.Errorf("unknown key produced %v, want none", got)
	}
}
