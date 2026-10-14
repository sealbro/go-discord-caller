package bot

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

func englishLocalizer() *i18n.Localizer {
	return i18n.MustBundle().For("en", "")
}

func TestRequireGuild(t *testing.T) {
	loc := englishLocalizer()

	id := snowflake.ID(42)
	got, errMsg := requireGuild(loc, &id)
	if errMsg != nil || got != id {
		t.Fatalf("requireGuild(&42) = %v, %+v; want 42, nil", got, errMsg)
	}

	_, errMsg = requireGuild(loc, nil)
	if errMsg == nil {
		t.Fatal("requireGuild(nil) returned no error message")
	}
	if errMsg.Flags != discord.MessageFlagEphemeral {
		t.Errorf("guild-only error flags = %v, want ephemeral", errMsg.Flags)
	}
	if errMsg.Content != loc.T("err.guild_only") {
		t.Errorf("guild-only error content = %q", errMsg.Content)
	}
}

func TestStatusEmoji(t *testing.T) {
	if statusEmoji(true) == statusEmoji(false) {
		t.Fatal("enabled and disabled speakers render the same emoji")
	}
}

func TestFormatBotChannels(t *testing.T) {
	loc := englishLocalizer()
	warnings := []manager.ChannelAccessWarning{
		{BotID: 1, ChannelID: 10},
		{BotID: 2, ChannelID: 20},
	}

	if got := formatAccessWarnings(loc, nil); got != "" {
		t.Errorf("formatAccessWarnings(nil) = %q, want empty", got)
	}
	if got := formatSkippedSpeakers(loc, nil); got != "" {
		t.Errorf("formatSkippedSpeakers(nil) = %q, want empty", got)
	}

	tests := []struct {
		name   string
		header string
		got    string
	}{
		{"access warnings", loc.T("permissions.warning_header"), formatAccessWarnings(loc, warnings)},
		{"skipped speakers", loc.T("raid.skipped_missing_channel"), formatSkippedSpeakers(loc, warnings)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.header + "\n- <@1> → <#10>\n- <@2> → <#20>"
			if tt.got != want {
				t.Fatalf("got %q, want %q", tt.got, want)
			}
		})
	}
}
