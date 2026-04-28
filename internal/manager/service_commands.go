package manager

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// SeedExistingSpeakers registers any pool bots already in each guild and ensures
// every guild has a persistent relay code. Call on startup and when the owner
// bot joins a new guild.
func (m *Service) SeedExistingSpeakers(guildIDs []snowflake.ID) {
	for _, guildID := range guildIDs {
		m.seedGuildSpeakers(guildID, m.ownerBotID)
		m.warmGuildCache(guildID)
	}
}

// HasAvailableToken reports whether the pool has at least one speaker bot
// that has not yet been added to the given guild.
func (m *Service) HasAvailableToken(guildID snowflake.ID) bool {
	_, ok := m.NextSpeakerID(guildID)
	return ok
}

// NextSpeakerID returns the Discord ApplicationID of the next pool speaker
// whose bot has NOT yet joined the guild.
func (m *Service) NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool) {
	m.mu.RLock()
	var registeredIDs map[snowflake.ID]struct{}
	if st := m.statuses[guildID]; st != nil {
		registeredIDs = make(map[snowflake.ID]struct{}, len(st.Speakers))
		for id := range st.Speakers {
			registeredIDs[id] = struct{}{}
		}
	}
	m.mu.RUnlock()

	for _, botUserID := range m.poolSvc.GetIDs() {
		if _, exists := registeredIDs[botUserID]; exists {
			continue // already registered
		}
		if m.isGuildMember(guildID, botUserID) {
			continue // already a guild member on Discord's side
		}
		return botUserID, true
	}
	return 0, false
}

// ToggleSpeaker enables or disables a speaker within a specific guild.
func (m *Service) ToggleSpeaker(guildID, speakerID snowflake.ID, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return fmt.Errorf("toggle speaker: guild %s not registered", guildID)
	}
	sp, exists := st.Speakers[speakerID]
	if !exists {
		return fmt.Errorf("toggle speaker: speaker %s not registered in guild %s", speakerID, guildID)
	}
	sp.Enabled = enabled
	return nil
}

// RemoveSpeaker removes a speaker from a guild's status when they leave the server.
func (m *Service) RemoveSpeaker(guildID, userID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return
	}
	delete(st.Speakers, userID)
	slog.Info("speaker removed from guild",
		slog.String("userID", userID.String()),
		slog.String("guildID", guildID.String()),
	)
}

// TrySeedMember checks whether a newly-joined guild member is an unregistered
// pool speaker bot and registers it if so.
func (m *Service) TrySeedMember(guildID, newUserID snowflake.ID) {
	newSpeaker, err := m.newSpeaker(newUserID)
	if err != nil {
		return // not a pool bot or user unresolvable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.statuses[guildID]
	if st == nil {
		return
	}
	existing, exists := st.Speakers[newUserID]
	if !exists {
		st.Speakers[newUserID] = newSpeaker
		slog.Info("member-join: registered speaker bot",
			slog.String("username", newSpeaker.Username),
			slog.String("guildID", guildID.String()),
		)
		return
	}
	// Refresh username, preserve per-guild Enabled state.
	existing.Username = newSpeaker.Username
	slog.Info("member-join: refreshed speaker username",
		slog.String("username", newSpeaker.Username),
		slog.String("guildID", guildID.String()),
	)
}

// OwnerBotID returns the owner bot's user ID.
func (m *Service) OwnerBotID() snowflake.ID { return m.ownerBotID }

// BindChannel binds a voice channel to a user (speaker or owner) in a guild.
func (m *Service) BindChannel(guildID, userID, channelID snowflake.ID) {
	m.store.BindChannel(guildID, userID, channelID)
}

// UnbindChannel removes the channel binding for a user in a guild.
func (m *Service) UnbindChannel(guildID, userID snowflake.ID) {
	m.store.UnbindChannel(guildID, userID)
}

// GetBoundChannel returns the bound voice channel for a user in a guild.
func (m *Service) GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetBoundChannel(guildID, userID)
}

// BindRole sets a Discord role binding of the given type for the guild.
func (m *Service) BindRole(guildID snowflake.ID, roleType store.RoleType, roleID snowflake.ID) {
	m.store.BindRole(guildID, roleType, roleID)
	slog.Info("role bound",
		slog.String("type", string(roleType)),
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
}

// UnbindRole removes a role binding of the given type for the guild.
func (m *Service) UnbindRole(guildID snowflake.ID, roleType store.RoleType) {
	m.store.UnbindRole(guildID, roleType)
	slog.Info("role unbound",
		slog.String("type", string(roleType)),
		slog.String("guildID", guildID.String()),
	)
}

// HasManagerRole reports whether any of the supplied role IDs matches the
// configured manager role for the guild.
func (m *Service) HasManagerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool {
	managerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeManager)
	if !ok {
		return false
	}
	return slices.Contains(memberRoleIDs, managerRoleID)
}

// HasCallerRole reports whether any of the supplied role IDs matches the
// configured caller role for the guild. If no caller role is set, returns true
// so that all users are allowed when the role is unconfigured.
func (m *Service) HasCallerRole(guildID snowflake.ID, memberRoleIDs []snowflake.ID) bool {
	callerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller)
	if !ok {
		return true // no restriction configured
	}
	return slices.Contains(memberRoleIDs, callerRoleID)
}

// GetStatus returns a safe, enriched value snapshot of the guild status.
// The returned value is fully owned by the caller; no locking required after return.
func (m *Service) GetStatus(guildID snowflake.ID) guild.Status {
	m.mu.RLock()
	snap := m.snapshotLocked(guildID)
	m.mu.RUnlock()

	m.enrichBindings(&snap, guildID)
	m.enrichRelayInfo(&snap, guildID)
	return snap
}

// enrichBindings populates channel bindings, role bindings, and guild metadata
// on the snapshot. Store has its own lock; no manager lock needed.
func (m *Service) enrichBindings(snap *guild.Status, guildID snowflake.ID) {
	for spID := range snap.Speakers {
		if chID, ok := m.store.GetBoundChannel(guildID, spID); ok {
			snap.BoundChannels[spID] = chID
		}
	}
	if roleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller); ok {
		snap.CallerRoleID = &roleID
	}
	if managerRoleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeManager); ok {
		snap.ManagerRoleID = &managerRoleID
	}
	snap.OwnerUserID = m.ownerBotID
	if chID, ok := m.store.GetBoundChannel(guildID, m.ownerBotID); ok {
		snap.BoundChannels[m.ownerBotID] = chID
	}
	if code, ok := m.store.GetAllyCode(guildID); ok {
		snap.AllyCode = code
	}
	if cachedGuild, ok := m.ownerClient.Caches.Guild(guildID); ok {
		snap.GuildName = cachedGuild.Name
	}
}

// enrichRelayInfo populates relay session info (host/guest guild names) on the snapshot.
func (m *Service) enrichRelayInfo(snap *guild.Status, guildID snowflake.ID) {
	if snap.Session == nil {
		return
	}
	relaySess, ok := m.sessions.GetByGuild(guildID)
	if !ok {
		return
	}
	if snap.Session.IsGuest {
		if hostGuild, ok := m.ownerClient.Caches.Guild(relaySess.HostGuildID); ok {
			snap.HostGuildName = hostGuild.Name
		}
	} else {
		for _, guestID := range relaySess.GuestGuildIDs() {
			name := guestID.String()
			if g, ok := m.ownerClient.Caches.Guild(guestID); ok {
				name = g.Name
			}
			snap.GuestGuildNames = append(snap.GuestGuildNames, name)
		}
	}
}

// HasActiveSession reports whether there is a running voice raid for the guild.
func (m *Service) HasActiveSession(guildID snowflake.ID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	return st != nil && st.HasActiveSession()
}
