package bot

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
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
