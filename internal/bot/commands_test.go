package bot

import (
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/manager"
)

// TestBotPermissions guards against accidental changes to the speaker bots'
// install URL bitmask.
func TestBotPermissions(t *testing.T) {
	const want discord.Permissions = 36701440
	if botPermissions != want {
		t.Errorf("botPermissions = %d, want %d; update the install URLs if the permission set changed intentionally", botPermissions, want)
	}
}

// ownerInviteDocs are the tracked files that hard-code the owner bot's invite
// URL. Paths are relative to this package.
var ownerInviteDocs = []string{
	"../../README.md",
	"../../docs/PERMISSIONS.md",
	"../../mkdocs.yml",
	"../../misc/website/index.md",
	"../../misc/website/free-shot-caller-bot.md",
}

// TestOwnerPermissions pins the owner bot's bitmask, which is botPermissions
// plus DEAFEN_MEMBERS and the server-facing permissions.
func TestOwnerPermissions(t *testing.T) {
	const want discord.Permissions = 351843765984576
	if ownerPermissions != want {
		t.Errorf("ownerPermissions = %d, want %d; also update every file in ownerInviteDocs", ownerPermissions, want)
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

// TestOwnerInviteURLsMatchPermissions checks the invite URLs published in the
// docs against ownerPermissions.
//
// The number is hard-coded in several tracked files, and they have already drifted
// once: when DEAFEN_MEMBERS was added, README.md was updated and the docs site
// was not, so every visitor following the "Try the hosted bot" button got an
// invite missing the permission and silently lost the optimisation. A comment
// reminding the next person to update them all is weaker than checking.
func TestOwnerInviteURLsMatchPermissions(t *testing.T) {
	wantParam := fmt.Sprintf("permissions=%d", ownerPermissions)
	staleParam := fmt.Sprintf("permissions=%d", botPermissions)

	for _, path := range ownerInviteDocs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			body := string(raw)
			if !strings.Contains(body, wantParam) {
				t.Errorf("%s has no owner invite URL with %q", path, wantParam)
			}
			// botPermissions is the speaker bitmask and the previous owner
			// value; either way it must not appear in an install link here.
			if strings.Contains(body, staleParam) {
				t.Errorf("%s still carries a stale invite URL with %q", path, staleParam)
			}
		})
	}
}

// permissionSpecs is every permission either bot requests, and which bot
// requests it. docs/PERMISSIONS.md documents the same table.
var permissionSpecs = []struct {
	flag    string
	perm    discord.Permissions
	owner   bool
	speaker bool
}{
	{"VIEW_CHANNEL", discord.PermissionViewChannel, true, true},
	{"CONNECT", discord.PermissionConnect, true, true},
	{"SPEAK", discord.PermissionSpeak, true, true},
	{"USE_VAD", discord.PermissionUseVAD, true, true},
	{"PRIORITY_SPEAKER", discord.PermissionPrioritySpeaker, true, true},
	{"DEAFEN_MEMBERS", discord.PermissionDeafenMembers, true, false},
	{"ADD_REACTIONS", discord.PermissionAddReactions, true, false},
	{"SEND_MESSAGES", discord.PermissionSendMessages, true, false},
	{"SEND_TTS_MESSAGES", discord.PermissionSendTTSMessages, true, false},
	{"SEND_VOICE_MESSAGES", discord.PermissionSendVoiceMessages, true, false},
	{"SET_VOICE_CHANNEL_STATUS", permissionSetVoiceChannelStatus, true, false},
}

func TestPermissionSpecs(t *testing.T) {
	var owner, speaker discord.Permissions
	for _, spec := range permissionSpecs {
		if got := ownerPermissions.Has(spec.perm); got != spec.owner {
			t.Errorf("owner bot requests %s = %v, want %v", spec.flag, got, spec.owner)
		}
		if got := botPermissions.Has(spec.perm); got != spec.speaker {
			t.Errorf("speaker bots request %s = %v, want %v", spec.flag, got, spec.speaker)
		}
		if spec.owner {
			owner |= spec.perm
		}
		if spec.speaker {
			speaker |= spec.perm
		}
	}
	if extra := ownerPermissions &^ owner; extra != 0 {
		t.Errorf("owner bot requests bits %b missing from permissionSpecs", extra)
	}
	if extra := botPermissions &^ speaker; extra != 0 {
		t.Errorf("speaker bots request bits %b missing from permissionSpecs", extra)
	}
}

// TestBotsRequestChannelAccess ties the install permissions to the check that
// blocks a raid: a bot installed without one of them could never pass it.
func TestBotsRequestChannelAccess(t *testing.T) {
	if !botPermissions.Has(manager.ChannelAccessPermissions) {
		t.Errorf("speaker bots request %d, missing channel access bits %b",
			botPermissions, manager.ChannelAccessPermissions&^botPermissions)
	}
	if !ownerPermissions.Has(manager.ChannelAccessPermissions) {
		t.Errorf("owner bot requests %d, missing channel access bits %b",
			ownerPermissions, manager.ChannelAccessPermissions&^ownerPermissions)
	}
}

type permissionDocRow struct {
	bit, value     string
	owner, speaker bool
}

// permissionDocRows reads the permission table in docs/PERMISSIONS.md, keyed
// by API flag.
func permissionDocRows(t *testing.T, doc string) map[string]permissionDocRow {
	t.Helper()
	rows := map[string]permissionDocRow{}
	for _, line := range strings.Split(doc, "\n") {
		cells := strings.Split(line, "|")
		// | Permission | `FLAG` | bit | `value` | owner | speaker | used for |
		if len(cells) != 9 || !strings.HasPrefix(strings.TrimSpace(cells[2]), "`") {
			continue
		}
		flag := strings.Trim(strings.TrimSpace(cells[2]), "`")
		if _, dup := rows[flag]; dup {
			t.Errorf("docs/PERMISSIONS.md lists %s twice", flag)
		}
		rows[flag] = permissionDocRow{
			bit:     strings.TrimSpace(cells[3]),
			value:   strings.Trim(strings.TrimSpace(cells[4]), "`"),
			owner:   strings.TrimSpace(cells[5]) == "✓",
			speaker: strings.TrimSpace(cells[6]) == "✓",
		}
	}
	return rows
}

func TestPermissionsDocMatchesCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/PERMISSIONS.md")
	if err != nil {
		t.Fatalf("read docs/PERMISSIONS.md: %v", err)
	}
	doc := string(raw)
	rows := permissionDocRows(t, doc)

	for _, spec := range permissionSpecs {
		row, ok := rows[spec.flag]
		if !ok {
			t.Errorf("docs/PERMISSIONS.md has no row for %s", spec.flag)
			continue
		}
		delete(rows, spec.flag)
		want := permissionDocRow{
			bit:     fmt.Sprint(bits.TrailingZeros64(uint64(spec.perm))),
			value:   fmt.Sprint(uint64(spec.perm)),
			owner:   spec.owner,
			speaker: spec.speaker,
		}
		if row != want {
			t.Errorf("docs/PERMISSIONS.md row for %s = %+v, want %+v", spec.flag, row, want)
		}
	}
	for flag := range rows {
		t.Errorf("docs/PERMISSIONS.md lists %s, which neither bot requests", flag)
	}

	for bot, perms := range map[string]discord.Permissions{"Owner": ownerPermissions, "Speaker": botPermissions} {
		if total := fmt.Sprintf("| %s | `%d` |", bot, perms); !strings.Contains(doc, total) {
			t.Errorf("docs/PERMISSIONS.md has no %q", total)
		}
	}
}
