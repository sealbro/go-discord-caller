package bot

import (
	"fmt"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// TestBotPermissions guards against accidental changes to the install URL
// permission bitmask. The constant must equal the original value so that
// existing bot invitations continue to work.
func TestBotPermissions(t *testing.T) {
	const want discord.Permissions = 391565762894144
	if botPermissions != want {
		t.Errorf("botPermissions = %d, want %d; update the install URLs if the permission set changed intentionally", botPermissions, want)
	}
}

// TestOwnerPermissions pins the owner bot's bitmask, which is botPermissions
// plus DEAFEN_MEMBERS. Both README install URLs hard-code this number, so a
// change here that is not mirrored there hands users an invite that silently
// disables the non-capture server-deafen.
func TestOwnerPermissions(t *testing.T) {
	const want discord.Permissions = 391565771282752
	if ownerPermissions != want {
		t.Errorf("ownerPermissions = %d, want %d; update the README install URLs if this changed intentionally", ownerPermissions, want)
	}
	if !ownerPermissions.Has(discord.PermissionDeafenMembers) {
		t.Error("ownerPermissions is missing DEAFEN_MEMBERS; non-capture speakers will never be deafened")
	}
	if botPermissions.Has(discord.PermissionDeafenMembers) {
		t.Error("speaker bots do not deafen anyone and must not request DEAFEN_MEMBERS")
	}
}

func TestInstallOwnerURL(t *testing.T) {
	id := snowflake.ID(123456789)
	got := installOwnerURL(id)
	want := fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d",
		id, ownerPermissions,
	)
	if got != want {
		t.Errorf("installOwnerURL() = %q, want %q", got, want)
	}
}

func TestInstallURL(t *testing.T) {
	clientID := snowflake.ID(123456789)
	guildID := snowflake.ID(987654321)
	got := installURL(clientID, guildID)
	want := fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d&guild_id=%s",
		clientID, botPermissions, guildID,
	)
	if got != want {
		t.Errorf("installURL() = %q, want %q", got, want)
	}
}
