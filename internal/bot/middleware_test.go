package bot

import (
	"slices"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func TestAuthorization(t *testing.T) {
	const (
		guildID     snowflake.ID = 1
		managerRole snowflake.ID = 100
		otherRole   snowflake.ID = 200
	)
	h := &CommandHandlers{manager: &fakeManager{
		hasManagerFn: func(g snowflake.ID, roles []snowflake.ID) bool {
			return g == guildID && slices.Contains(roles, managerRole)
		},
	}}
	member := func(perms discord.Permissions, roles ...snowflake.ID) *discord.ResolvedMember {
		return &discord.ResolvedMember{
			Member:      discord.Member{RoleIDs: roles},
			Permissions: perms,
		}
	}

	tests := []struct {
		name        string
		member      *discord.ResolvedMember
		wantAdmin   bool
		wantManager bool
	}{
		{"nil member", nil, false, false},
		{"no permissions or roles", member(0, otherRole), false, false},
		{"administrator", member(discord.PermissionAdministrator), true, false},
		{"manage server", member(discord.PermissionManageGuild), false, true},
		{"manager role", member(0, otherRole, managerRole), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.isAdminAuthorized(guildID, tt.member); got != tt.wantAdmin {
				t.Errorf("isAdminAuthorized = %v, want %v", got, tt.wantAdmin)
			}
			if got := h.isManagerAuthorized(guildID, tt.member); got != tt.wantManager {
				t.Errorf("isManagerAuthorized = %v, want %v", got, tt.wantManager)
			}
		})
	}
}
