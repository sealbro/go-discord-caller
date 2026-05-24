// Package i18n provides locale-aware string lookup for user-facing messages.
//
// The package embeds YAML translation files under locales/ at compile time and
// exposes a Localizer per Discord interaction. The English bundle (en.yaml) is
// the source of truth: bundle_test.go fails if any other locale is missing a
// key present in en.yaml.
package i18n

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

//go:embed locales/*.yaml
var localesFS embed.FS

// DefaultLocale is the fallback locale used when no other lookup succeeds.
const DefaultLocale = "en"

// Bundle holds the loaded i18n.Bundle plus the list of locale tags loaded
// from the embedded filesystem.
type Bundle struct {
	b       *i18n.Bundle
	tags    []language.Tag
	keysMu  sync.RWMutex
	allKeys []string // populated lazily for tests
}

// NewBundle loads every locales/*.yaml file from the embedded FS into a
// fresh bundle. The default locale (en) must be present.
func NewBundle() (*Bundle, error) {
	bundle := i18n.NewBundle(language.MustParse(DefaultLocale))
	bundle.RegisterUnmarshalFunc("yaml", yaml.Unmarshal)

	entries, err := fs.ReadDir(localesFS, "locales")
	if err != nil {
		return nil, fmt.Errorf("i18n: read embedded locales dir: %w", err)
	}

	var tags []language.Tag
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := path.Join("locales", e.Name())
		data, err := localesFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("i18n: read %s: %w", name, err)
		}
		msgFile, err := bundle.ParseMessageFileBytes(data, name)
		if err != nil {
			return nil, fmt.Errorf("i18n: parse %s: %w", name, err)
		}
		tags = append(tags, msgFile.Tag)
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("i18n: no locale files found")
	}

	return &Bundle{b: bundle, tags: tags}, nil
}

// Tags returns the list of language tags loaded into the bundle.
func (b *Bundle) Tags() []language.Tag {
	out := make([]language.Tag, len(b.tags))
	copy(out, b.tags)
	return out
}

// MustBundle is a convenience constructor that panics on load failure.
// Intended for use in init code where a missing/broken locale file is fatal.
func MustBundle() *Bundle {
	b, err := NewBundle()
	if err != nil {
		panic(err)
	}
	return b
}
