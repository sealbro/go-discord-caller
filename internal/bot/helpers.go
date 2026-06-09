package bot

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

func requireGuild(loc *i18n.Localizer, guildID *snowflake.ID) (snowflake.ID, *discord.MessageCreate) {
	if guildID == nil {
		msg := ephemeral(loc.T("err.guild_only"))
		return 0, &msg
	}
	return *guildID, nil
}

func ephemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral}
}

func statusEmoji(enabled bool) string {
	if enabled {
		return "🔊"
	}
	return "🔇"
}

// formatAccessWarnings builds a Discord-formatted warning block from the access check
// results. Returns "" when there are no warnings.
func formatAccessWarnings(loc *i18n.Localizer, warnings []manager.ChannelAccessWarning) string {
	if len(warnings) == 0 {
		return ""
	}
	msg := loc.T("permissions.warning_header")
	for _, w := range warnings {
		msg += fmt.Sprintf("\n- <@%s> → <#%s>", w.BotID, w.ChannelID)
	}
	return msg
}

// botPermissions is the Discord permission bitmask required by speaker bots.
// Bit 48 is an unnamed/reserved Discord permission retained from the original
// install URL; remove it if Discord revokes or reassigns the bit.
const botPermissions discord.Permissions = discord.PermissionAddReactions |
	discord.PermissionPrioritySpeaker |
	discord.PermissionSendMessages |
	discord.PermissionSendTTSMessages |
	discord.PermissionUseExternalEmojis |
	discord.PermissionConnect |
	discord.PermissionSpeak |
	discord.PermissionUseVAD |
	discord.PermissionUseApplicationCommands |
	discord.PermissionUseExternalStickers |
	discord.PermissionUseSoundboard |
	discord.PermissionUseExternalSounds |
	discord.PermissionSendVoiceMessages |
	1<<48 // reserved/unnamed bit present in original install URL

func installOwnerURL(clientID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d",
		clientID, botPermissions,
	)
}

func installURL(clientID snowflake.ID, guildID snowflake.ID) string {
	return fmt.Sprintf(
		"https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d&guild_id=%s",
		clientID, botPermissions, guildID,
	)
}
