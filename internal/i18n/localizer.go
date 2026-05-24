package i18n

import (
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/nicksnyder/go-i18n/v2/i18n"
)

// Localizer is a thin wrapper around go-i18n's Localizer that swallows lookup
// errors and falls back to the key itself (logged at warn level) so that a
// missing translation never crashes a handler — at worst the user sees the
// raw key, which is enough to spot the problem in production.
type Localizer struct {
	loc    *i18n.Localizer
	locale string // resolved bundle locale (e.g. "en", "ru")
}

// Locale returns the resolved bundle locale code for this localizer.
func (l *Localizer) Locale() string { return l.locale }

// For returns a Localizer using guildLocale when non-empty, otherwise the
// interaction locale, otherwise DefaultLocale. Unknown locales fall back to
// DefaultLocale silently.
//
// Resolution order:
//  1. guildLocale (admin-pinned via /setup dropdown)
//  2. interactionLocale (Discord client locale of the invoking user)
//  3. DefaultLocale ("en")
func (b *Bundle) For(guildLocale string, interactionLocale discord.Locale) *Localizer {
	resolved := DefaultLocale
	candidates := []string{guildLocale, bundleLocaleFromDiscord(interactionLocale), DefaultLocale}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if b.hasLocale(c) {
			resolved = c
			break
		}
	}
	return &Localizer{
		loc:    i18n.NewLocalizer(b.b, resolved, DefaultLocale),
		locale: resolved,
	}
}

// hasLocale reports whether the bundle has the named locale loaded.
func (b *Bundle) hasLocale(code string) bool {
	for _, tag := range b.tags {
		if tag.String() == code {
			return true
		}
	}
	return false
}

// T looks up a key and substitutes optional template variables.
// Variables are passed as alternating name/value pairs:
//
//	loc.T("raid.joined", "Code", "ABC123", "Mode", "Many Callers")
//
// For pluralised keys, pass a "Count" pair — go-i18n picks the right plural form:
//
//	loc.T("status.speakers_count", "Count", n)
//
// On lookup failure the key itself is returned so callers always get a non-empty string.
func (l *Localizer) T(key string, args ...any) string {
	cfg := &i18n.LocalizeConfig{MessageID: key}
	if len(args) > 0 {
		data := make(map[string]any, len(args)/2)
		var pluralCount any
		for i := 0; i+1 < len(args); i += 2 {
			name, ok := args[i].(string)
			if !ok {
				slog.Warn("i18n: non-string template var name", slog.String("key", key))
				continue
			}
			data[name] = args[i+1]
			if name == "Count" {
				pluralCount = args[i+1]
			}
		}
		cfg.TemplateData = data
		if pluralCount != nil {
			cfg.PluralCount = pluralCount
		}
	}
	msg, err := l.loc.Localize(cfg)
	if err != nil {
		slog.Warn("i18n: missing translation", slog.String("key", key), slog.String("locale", l.locale), slog.Any("err", err))
		return key
	}
	return msg
}
