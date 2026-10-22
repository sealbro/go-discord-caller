package bot

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/i18n"
)

func bindRolesButton(t *testing.T, components []discord.LayoutComponent) discord.ButtonComponent {
	t.Helper()
	for _, c := range components {
		row, ok := c.(discord.ActionRowComponent)
		if !ok {
			continue
		}
		for _, ic := range row.Components {
			if b, ok := ic.(discord.ButtonComponent); ok && b.CustomID == "/roles/menu" {
				return b
			}
		}
	}
	t.Fatal("main setup message has no Bind Roles button")
	return discord.ButtonComponent{}
}

// A raid cannot start without a capture role, so the setup menu has to point
// the admin at the step that is still missing.
func TestMainSetupFlagsMissingCaptureRole(t *testing.T) {
	const guildID snowflake.ID = 1
	role := snowflake.ID(100)
	loc := i18n.MustBundle().For("", discord.LocaleEnglishUS)

	tests := []struct {
		name      string
		roleID    *snowflake.ID
		wantStyle discord.ButtonStyle
	}{
		{"capture role missing", nil, discord.ButtonStyleDanger},
		{"capture role bound", &role, discord.ButtonStylePrimary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &CommandHandlers{manager: &fakeManager{
				statusFn: func(snowflake.ID) guild.Status {
					st := guild.NewStatus(guildID, 0)
					st.CallerRoleID = tt.roleID
					return *st
				},
			}}

			_, components := h.buildMainSetupMessage(guildID, loc)

			if got := bindRolesButton(t, components).Style; got != tt.wantStyle {
				t.Errorf("Bind Roles button style = %v, want %v", got, tt.wantStyle)
			}
		})
	}
}
