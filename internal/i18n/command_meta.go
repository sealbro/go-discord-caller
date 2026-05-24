package i18n

import "github.com/disgoorg/disgo/discord"

// DescriptionLocalizations returns the per-locale translation of the given key,
// suitable for use as discord.SlashCommandCreate.DescriptionLocalizations or
// option/choice DescriptionLocalizations.
//
// The default-locale value is not included in the map (Discord uses the
// top-level Description field for that). Unmapped bundle locales are skipped.
func (b *Bundle) DescriptionLocalizations(key string) map[discord.Locale]string {
	out := map[discord.Locale]string{}
	for _, bl := range SupportedBundleLocales() {
		if bl == DefaultLocale {
			continue
		}
		if !b.hasLocale(bl) {
			continue
		}
		loc := b.For(bl, "")
		val := loc.T(key)
		if val == "" || val == key {
			continue
		}
		for _, d := range DiscordLocalesFor(bl) {
			out[d] = val
		}
	}
	return out
}

// NameLocalizations returns per-locale translations for use as
// SlashCommandCreate.NameLocalizations or option NameLocalizations. By default
// we keep slash command names in English, but option choice labels (e.g. "One
// Caller") benefit from localization.
func (b *Bundle) NameLocalizations(key string) map[discord.Locale]string {
	// Same shape as DescriptionLocalizations — Discord's API treats them identically.
	return b.DescriptionLocalizations(key)
}
