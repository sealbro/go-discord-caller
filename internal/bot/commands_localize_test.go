package bot

import (
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/sealbro/go-discord-caller/internal/i18n"
)

// Discord's character limits for slash command metadata.
// See https://discord.com/developers/docs/interactions/application-commands.
const (
	maxCommandNameLen = 32  // lowercase, 1-32
	maxOptionNameLen  = 32  // 1-32
	maxChoiceNameLen  = 100 // 1-100
	maxDescriptionLen = 100 // 1-100 (commands, options)
)

// TestBuildCommands_DiscordLimits ensures every name/description in BuildCommands
// — including all localized variants — fits Discord's documented length limits.
// Catches translation overruns before they reach Discord's command sync (which
// otherwise returns BASE_TYPE_BAD_LENGTH and refuses to register the bot).
func TestBuildCommands_DiscordLimits(t *testing.T) {
	bundle, err := i18n.NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	cmds := BuildCommands(bundle)
	if len(cmds) == 0 {
		t.Fatal("BuildCommands returned no commands")
	}

	for _, cmd := range cmds {
		sc, ok := cmd.(discord.SlashCommandCreate)
		if !ok {
			t.Errorf("unexpected command type %T", cmd)
			continue
		}
		validateLen(t, "cmd "+sc.Name+" .Name", sc.Name, 1, maxCommandNameLen)
		validateLen(t, "cmd "+sc.Name+" .Description", sc.Description, 1, maxDescriptionLen)
		validateLocalizations(t, "cmd "+sc.Name+" .DescriptionLocalizations", sc.DescriptionLocalizations, maxDescriptionLen)

		for i, opt := range sc.Options {
			path := "cmd " + sc.Name + " opt " + strconv.Itoa(i)
			switch o := opt.(type) {
			case discord.ApplicationCommandOptionString:
				validateLen(t, path+" .Name", o.Name, 1, maxOptionNameLen)
				validateLen(t, path+" .Description", o.Description, 1, maxDescriptionLen)
				validateLocalizations(t, path+" .DescriptionLocalizations", o.DescriptionLocalizations, maxDescriptionLen)
				validateLocalizations(t, path+" .NameLocalizations", o.NameLocalizations, maxOptionNameLen)
				for j, ch := range o.Choices {
					chPath := path + " choice " + strconv.Itoa(j)
					validateLen(t, chPath+" .Name", ch.Name, 1, maxChoiceNameLen)
					validateLocalizations(t, chPath+" .NameLocalizations", ch.NameLocalizations, maxChoiceNameLen)
				}
			case discord.ApplicationCommandOptionRole:
				validateLen(t, path+" .Name", o.Name, 1, maxOptionNameLen)
				validateLen(t, path+" .Description", o.Description, 1, maxDescriptionLen)
				validateLocalizations(t, path+" .DescriptionLocalizations", o.DescriptionLocalizations, maxDescriptionLen)
				validateLocalizations(t, path+" .NameLocalizations", o.NameLocalizations, maxOptionNameLen)
			default:
				t.Errorf("%s: unhandled option type %T — extend the test", path, opt)
			}
		}
	}
}

func validateLen(t *testing.T, path, value string, minN, maxN int) {
	t.Helper()
	n := utf8.RuneCountInString(value)
	if n < minN || n > maxN {
		t.Errorf("%s: length %d outside Discord limit [%d, %d]; value=%q", path, n, minN, maxN, value)
	}
}

func validateLocalizations(t *testing.T, path string, m map[discord.Locale]string, maxN int) {
	t.Helper()
	for locale, value := range m {
		validateLen(t, path+"["+string(locale)+"]", value, 1, maxN)
	}
}
