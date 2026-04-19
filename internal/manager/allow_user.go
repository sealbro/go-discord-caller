package manager

import (
	"context"
	"log/slog"
	"slices"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// buildAllowUserFilter returns a filter function that decides whether a user's
// voice frames should be captured. When a caller role is bound it filters by
// that role; otherwise all non-bot users are allowed.
//
// The filter is built once per session and shared across all voice connections
// (owner and speakers). Call prefetchChannelMembers separately for each
// connection to warm the member cache before the filter is first evaluated.
//
// NOTE: The roleID is captured at build time. If an admin changes the capture
// role mid-raid, the running session continues using the original role until
// the raid is restarted.
func (m *Service) buildAllowUserFilter(guildID snowflake.ID) func(snowflake.ID) bool {
	caches := m.ownerClient.Caches

	roleID, hasRole := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	if !hasRole {
		// No role filter — allow all non-bot users.
		return func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			return ok && !member.User.Bot
		}
	}

	slog.Info("role filter active", slog.String("guildID", guildID.String()), slog.String("roleID", roleID.String()))

	return func(userID snowflake.ID) bool {
		member, ok := caches.Member(guildID, userID)
		if !ok {
			return false
		}
		if m.test.IsTestBot(userID) {
			return true
		}
		if member.User.Bot {
			return false
		}
		return slices.Contains(member.RoleIDs, roleID)
	}
}

// prefetchChannelMembers requests full member data for every user currently in
// conn's voice channel via a single RequestMembers gateway op. Discord responds
// with GUILD_MEMBERS_CHUNK events that populate the cache with complete RoleIDs,
// replacing any partial entries written by earlier VOICE_STATE_UPDATE events.
func (m *Service) prefetchChannelMembers(ctx context.Context, conn voice.Conn, botUserID, guildID snowflake.ID) {
	chID := conn.ChannelID()
	if chID == nil {
		return
	}
	var userIDs []snowflake.ID
	for vs := range m.ownerClient.Caches.VoiceStates(guildID) {
		if vs.ChannelID != nil && *vs.ChannelID == *chID && vs.UserID != botUserID {
			userIDs = append(userIDs, vs.UserID)
		}
	}
	if len(userIDs) > 0 {
		if err := m.ownerClient.RequestMembers(ctx, guildID, false, "", userIDs...); err != nil {
			slog.WarnContext(ctx, "prefetchChannelMembers: RequestMembers failed", slog.Any("err", err))
		}
	}
}
