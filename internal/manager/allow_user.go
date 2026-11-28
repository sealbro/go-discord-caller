package manager

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
	"go.opentelemetry.io/otel/attribute"
)

// AllowFilter decides per frame whose audio a session captures: the members
// the session's voice view places in voice holding the caller role. It reads
// only the view, never the caches, so a frame costs an atomic load and a map
// lookup.
type AllowFilter struct {
	view *viewProbe
	// roleID is the guild's caller role when the raid started; zero when none
	// is bound, which admits nobody.
	roleID snowflake.ID
	// lastAdmitted is the unix-nano time of the last frame Admit let through.
	lastAdmitted atomic.Int64
}

// RoleID returns the caller role ID this filter was built for. Returns 0 when
// no caller role is configured.
func (f *AllowFilter) RoleID() snowflake.ID { return f.roleID }

// Check returns true if userID is allowed to send audio.
func (f *AllowFilter) Check(userID snowflake.ID) bool {
	return f.view.holdsRole(userID, f.roleID)
}

// Admit is Check for a frame that has arrived: it also records the arrival.
func (f *AllowFilter) Admit(userID snowflake.ID) bool {
	if !f.Check(userID) {
		return false
	}
	f.lastAdmitted.Store(time.Now().UnixNano())
	return true
}

// LastAdmitted returns when Admit last let a frame through, or the zero time.
func (f *AllowFilter) LastAdmitted() time.Time {
	n := f.lastAdmitted.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// buildAllowUserFilter creates the AllowFilter for a raid in guildID, answering
// from view. Call prefetchChannelMembers for each connection so members whose
// roles the caches lack are fetched before their first frame.
func (m *Service) buildAllowUserFilter(guildID snowflake.ID, view *viewProbe) *AllowFilter {
	roleID, hasRole := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	if hasRole {
		slog.Info("role filter active", slog.String("guildID", guildID.String()), slog.String("roleID", roleID.String()))
	}
	return &AllowFilter{view: view, roleID: roleID}
}

// NotifyMemberUpdate brings the raid's view of voice, and with it the capture
// filter, up to date after a member's roles changed or they joined voice.
//
// Also nudges the auto-router (if any) so that a mid-session role grant /
// revoke on a user currently in a captured voice channel flips C(channel) and
// re-runs the cascade. Without this hop the router would only see the change
// when the user next moved between channels.
func (m *Service) NotifyMemberUpdate(guildID snowflake.ID, member discord.Member) {
	m.RefreshVoiceView(guildID)
	m.nudgeRouterFor(guildID, member.User.ID)
}

// membersFetched is NotifyMemberUpdate for a batch: one refresh covers them all.
func (m *Service) membersFetched(guildID snowflake.ID, members []discord.Member) {
	m.RefreshVoiceView(guildID)
	for _, member := range members {
		m.nudgeRouterFor(guildID, member.User.ID)
	}
}

func (m *Service) nudgeRouterFor(guildID, userID snowflake.ID) {
	m.mu.RLock()
	var router guild.AutoRouter
	if st := m.statuses[guildID]; st != nil && st.Session != nil {
		router = st.Session.AutoRouter
	}
	m.mu.RUnlock()

	if router == nil {
		return
	}
	// Look up the user's current voice channel from the cache; only nudge
	// the router when the user is in voice (a role change for someone not in
	// voice has no effect on per-channel caller counts).
	for vs := range m.ownerClient.Caches.VoiceStates(guildID) {
		if vs.UserID == userID && vs.ChannelID != nil {
			router.Debounce(*vs.ChannelID)
			return
		}
	}
}

// prefetchChannelMembers fetches full member data for every user currently in
// conn's voice channel and pre-populates the member cache with complete RoleIDs.
// Uses MemberChunkingManager.RequestMembers so the nonce is tracked and
// GUILD_MEMBERS_CHUNK responses are actually stored in the cache — bot.Client.RequestMembers
// sends the op with an empty nonce, causing HandleChunk to discard the response.
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
	if len(userIDs) == 0 {
		return
	}
	reqCtx, endPhase := startPhase(ctx, "voice.members.prefetch",
		botAttr(botUserID), attribute.Int("user.count", len(userIDs)))
	members, err := m.ownerClient.MemberChunkingManager.RequestMembers(reqCtx, guildID, userIDs...)
	endPhase(err)
	if err != nil {
		slog.WarnContext(ctx, "prefetchChannelMembers: RequestMembers failed", slog.Any("err", err))
		return
	}
	m.membersFetched(guildID, members)
}
