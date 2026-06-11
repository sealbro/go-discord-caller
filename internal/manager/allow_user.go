package manager

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// AllowFilter is a per-session, event-updated cache of per-user allow decisions.
// The hot path (Check) does a single sync.Map load; Set is called by event handlers
// on member updates so the hot path stays off the shared disgo cache mutex.
type AllowFilter struct {
	allowed  sync.Map // snowflake.ID → bool
	fallback func(snowflake.ID) bool
	metrics  telemetry.OpusRecorder
	// roleID is the configured caller role for the guild this filter belongs
	// to. Zero when no caller role is configured (the filter then allows all
	// non-bot users). Exposed via RoleID() so the auto-router can capture
	// it at session start without a second store lookup.
	roleID snowflake.ID
}

// RoleID returns the caller role ID this filter was built for. Returns 0 when
// no caller role is configured. The router uses this to compute per-channel
// caller counts; a zero value means "every non-bot is a caller" (consistent
// with Check semantics).
func (f *AllowFilter) RoleID() snowflake.ID { return f.roleID }

// Check returns true if userID is allowed to send audio.
// Uses the local map when a decision is cached, otherwise falls back to the
// disgo member cache, records the lookup duration, and memoises the result.
func (f *AllowFilter) Check(userID snowflake.ID) bool {
	if v, ok := f.allowed.Load(userID); ok {
		return v.(bool)
	}
	start := time.Now()
	result := f.fallback(userID)
	f.metrics.RecordAllowUser(float64(time.Since(start).Microseconds()) / 1000.0)
	f.allowed.Store(userID, result)
	return result
}

// Set stores an explicit allow decision for userID.
// Called from event handlers (onGuildMemberUpdate, onVoiceJoin) so the next
// frame from that user skips the fallback lookup entirely.
func (f *AllowFilter) Set(userID snowflake.ID, allowed bool) {
	f.allowed.Store(userID, allowed)
}

// buildAllowUserFilter creates an AllowFilter for guildID.
// The filter is built once per session and shared across all voice connections.
// Call prefetchChannelMembers separately for each connection to warm the disgo
// member cache before the first frame arrives.
func (m *Service) buildAllowUserFilter(guildID snowflake.ID) *AllowFilter {
	caches := m.ownerClient.Caches
	rec := m.metrics.Opus.For(guildID.String())

	roleID, hasRole := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	if !hasRole {
		return &AllowFilter{
			metrics: rec,
			fallback: func(userID snowflake.ID) bool {
				member, ok := caches.Member(guildID, userID)
				return ok && !m.IsBot(member.User)
			},
		}
	}

	slog.Info("role filter active", slog.String("guildID", guildID.String()), slog.String("roleID", roleID.String()))

	return &AllowFilter{
		metrics: rec,
		roleID:  roleID,
		fallback: func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			if !ok || m.IsBot(member.User) {
				return false
			}
			return slices.Contains(member.RoleIDs, roleID)
		},
	}
}

// NotifyMemberUpdate pushes a fresh allow decision into the active session's
// AllowFilter. Called by event handlers when a member's roles change or they
// join a voice channel; no-op when there is no active session.
func (m *Service) NotifyMemberUpdate(guildID snowflake.ID, member discord.Member) {
	m.mu.RLock()
	st := m.statuses[guildID]
	if st == nil || st.Session == nil || st.Session.AllowFilter == nil {
		m.mu.RUnlock()
		return
	}
	filter := st.Session.AllowFilter
	m.mu.RUnlock()

	roleID, hasRole := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	allowed := !m.IsBot(member.User)
	if hasRole && allowed {
		allowed = slices.Contains(member.RoleIDs, roleID)
	}
	filter.Set(member.User.ID, allowed)
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
	members, err := m.ownerClient.MemberChunkingManager.RequestMembers(ctx, guildID, userIDs...)
	if err != nil {
		slog.WarnContext(ctx, "prefetchChannelMembers: RequestMembers failed", slog.Any("err", err))
		return
	}
	for _, member := range members {
		m.NotifyMemberUpdate(guildID, member)
	}
}
