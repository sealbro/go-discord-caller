package i18n

import "github.com/disgoorg/disgo/discord"

// discordLocaleToBundle maps a Discord client locale code to the bundle locale
// it should resolve to. Locales not in this map fall back to DefaultLocale.
var discordLocaleToBundle = map[discord.Locale]string{
	discord.LocaleEnglishUS:    "en",
	discord.LocaleEnglishGB:    "en",
	discord.LocaleRussian:      "ru",
	discord.LocaleSpanishES:    "es",
	discord.LocaleSpanishLATAM: "es",
	discord.LocaleGerman:       "de",
	discord.LocaleFrench:       "fr",
	discord.LocalePortugueseBR: "pt",
	discord.LocalePolish:       "pl",
	discord.LocaleTurkish:      "tr",
}

// bundleLocaleFromDiscord returns the bundle locale code for the given Discord
// locale, or "" when the locale is not mapped.
func bundleLocaleFromDiscord(d discord.Locale) string {
	if d == "" {
		return ""
	}
	if bl, ok := discordLocaleToBundle[d]; ok {
		return bl
	}
	return ""
}

// SupportedBundleLocales returns the set of bundle locale codes that the
// /setup dropdown should expose. Order matches the dropdown rendering.
func SupportedBundleLocales() []string {
	return []string{"en", "es", "de", "fr", "pt", "pl", "ru", "tr"}
}

// DisplayName returns a human-readable native-language name for a bundle locale.
// Used as the label for /setup/locale dropdown options. Always rendered in the
// locale's own language so users can recognise their own language regardless of
// the bot's current language setting.
func DisplayName(bundleLocale string) string {
	switch bundleLocale {
	case "en":
		return "🇬🇧 English"
	case "es":
		return "🇪🇸 Español"
	case "de":
		return "🇩🇪 Deutsch"
	case "fr":
		return "🇫🇷 Français"
	case "pt":
		return "🇧🇷 Português"
	case "pl":
		return "🇵🇱 Polski"
	case "ru":
		return "🇷🇺 Русский"
	case "tr":
		return "🇹🇷 Turkish"
	default:
		return bundleLocale
	}
}

// DiscordLocalesFor returns all Discord locale codes that map to the given
// bundle locale. Used to populate Discord's name_localizations /
// description_localizations maps for slash commands.
func DiscordLocalesFor(bundleLocale string) []discord.Locale {
	var out []discord.Locale
	for d, bl := range discordLocaleToBundle {
		if bl == bundleLocale {
			out = append(out, d)
		}
	}
	return out
}
