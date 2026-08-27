package i18n

import (
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestBundleParity asserts that every key declared in en.yaml is present in all
// other locale files. Catches missing translations at CI time instead of in
// production where the localizer would silently fall back to English (or to
// the raw key).
func TestBundleParity(t *testing.T) {
	enKeys, err := loadLocaleKeys("en.yaml")
	if err != nil {
		t.Fatalf("load en.yaml: %v", err)
	}
	sort.Strings(enKeys)

	entries, err := fs.ReadDir(localesFS, "locales")
	if err != nil {
		t.Fatalf("read locales dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "en.yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			keys, err := loadLocaleKeys(e.Name())
			if err != nil {
				t.Fatalf("load %s: %v", e.Name(), err)
			}
			got := make(map[string]struct{}, len(keys))
			for _, k := range keys {
				got[k] = struct{}{}
			}
			var missing []string
			for _, k := range enKeys {
				if _, ok := got[k]; !ok {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s missing %d keys: %v", e.Name(), len(missing), missing)
			}
		})
	}
}

// TestCommandDescriptionLengths asserts every "cmd.*.description" string (and
// option descriptions) fits Discord's 1-100 char limit for
// description_localizations. A too-long localized description makes Discord
// reject the entire application command sync with 50035 Invalid Form Body,
// breaking slash commands for every guild (issue #58).
func TestCommandDescriptionLengths(t *testing.T) {
	entries, err := fs.ReadDir(localesFS, "locales")
	if err != nil {
		t.Fatalf("read locales dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			data, err := localesFS.ReadFile(path.Join("locales", e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			var raw map[string]any
			if err := yaml.Unmarshal(data, &raw); err != nil {
				t.Fatalf("unmarshal %s: %v", e.Name(), err)
			}
			for k, v := range raw {
				if !strings.HasPrefix(k, "cmd.") || !strings.HasSuffix(k, ".description") {
					continue
				}
				s, ok := v.(string)
				if !ok {
					continue
				}
				n := len([]rune(s))
				if n < 1 || n > 100 {
					t.Errorf("%s: %q is %d chars, want 1-100", k, s, n)
				}
			}
		})
	}
}

// TestNewBundleLoads ensures NewBundle parses every embedded locale file.
func TestNewBundleLoads(t *testing.T) {
	b, err := NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	tags := b.Tags()
	if len(tags) < 2 {
		t.Fatalf("expected at least en+ru locales, got %d", len(tags))
	}
	// Spot-check that the English bundle resolves a well-known key.
	en := b.For("", "")
	if got := en.T("err.guild_only"); got == "err.guild_only" {
		t.Errorf("en localizer returned raw key for err.guild_only")
	}
}

// loadLocaleKeys returns every flat key path defined in a locale YAML file,
// flattening nested plural maps (one/few/many/other) to just the top-level key.
func loadLocaleKeys(name string) ([]string, error) {
	data, err := localesFS.ReadFile(path.Join("locales", name))
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	return keys, nil
}
