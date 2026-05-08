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

func TestInstallOwnerURL(t *testing.T) {
	id := snowflake.ID(123456789)
	got := installOwnerURL(id)
	want := fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d",
		id, botPermissions,
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
