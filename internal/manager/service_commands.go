package manager

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
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
		return fmt.Errorf("guild %s has no registered status", guildID)
	}
	sp, exists := st.Speakers[speakerID]
	if !exists {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
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
	if guild, ok := m.ownerClient.Caches.Guild(guildID); ok {
		snap.GuildName = guild.Name
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

// ReconnectBotChannel reconnects a bot to its bound voice channel in the given guild.
// Called when a bot's voice connection drops or is moved away during an active session.
// No-op if there is no active session, the bot has no bound channel, or a reconnect
// for this bot is already in flight (prevents the leave→reconnect→leave→... loop).
func (m *Service) ReconnectBotChannel(ctx context.Context, guildID, botUserID snowflake.ID) {
	// Guard: one reconnect attempt per (guild, bot) at a time.
	// Calling Leave below fires another GuildVoiceLeave which would re-enter here;
	// the LoadOrStore makes that second call a no-op.
	key := botKey{guildID, botUserID}
	if _, loaded := m.reconnecting.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	defer m.reconnecting.Delete(key)

	if !m.HasActiveSession(guildID) {
		return
	}
	channelID, ok := m.store.GetBoundChannel(guildID, botUserID)
	if !ok || channelID == 0 {
		return
	}
	var gv pool.GuildVoice
	if botUserID == m.ownerBotID {
		gv = m.ownerVoice(guildID)
	} else {
		var found bool
		gv, found = m.speakerVoice(guildID, botUserID)
		if !found {
			return
		}
	}

	// Close the existing (possibly broken) voice connection so conn.Open starts
	// fresh instead of re-using stale internal state that causes a timeout.
	leaveCtx, leaveCancel := context.WithTimeout(ctx, voiceLeaveTimeout)
	gv.Leave(leaveCtx, guildID)
	leaveCancel()

	if !m.HasActiveSession(guildID) {
		return // session ended while we were closing
	}

	reconnCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := gv.Join(reconnCtx, guildID)
	if err != nil {
		// Single retry after a short backoff to handle transient failures
		// (Discord rate limits, brief network interruptions). The reconnect
		// guard stays held so a concurrent leave event does not race us.
		slog.WarnContext(ctx, "reconnect: first join attempt failed, retrying in 2s",
			slog.String("guildID", guildID.String()),
			slog.String("botUserID", botUserID.String()),
			slog.Any("err", err),
		)
		select {
		case <-reconnCtx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if !m.HasActiveSession(guildID) {
			return // session ended during backoff
		}
		conn, err = gv.Join(reconnCtx, guildID)
		if err != nil {
			slog.WarnContext(ctx, "reconnect: failed to rejoin bound channel",
				slog.String("guildID", guildID.String()),
				slog.String("botUserID", botUserID.String()),
				slog.String("channelID", channelID.String()),
				slog.Any("err", err),
			)
			return
		}
	}
	// Re-apply voice provider/receiver to the new conn so audio flows again.
	// Pass ctx (the reconnect context) so the applier's FrameDroppers use a live,
	// uncancelled context rather than the stale session-start context.
	if applier, ok := m.loadApplier(guildID, botUserID); ok {
		applier(ctx, conn)
	}
	slog.InfoContext(ctx, "reconnect: bot rejoined bound channel",
		slog.String("guildID", guildID.String()),
		slog.String("botUserID", botUserID.String()),
		slog.String("channelID", channelID.String()),
	)
}

// storeApplier saves a reconnectApplier for the given guild+bot pair.
func (m *Service) storeApplier(guildID, botUserID snowflake.ID, a reconnectApplier) {
	m.reconnectAppliers.Store(botKey{guildID, botUserID}, a)
}

// loadApplier retrieves the reconnectApplier for the given guild+bot pair.
func (m *Service) loadApplier(guildID, botUserID snowflake.ID) (reconnectApplier, bool) {
	v, ok := m.reconnectAppliers.Load(botKey{guildID, botUserID})
	if !ok {
		return nil, false
	}
	return v.(reconnectApplier), true
}

// clearAppliers removes all reconnect appliers for a guild (call on session teardown).
// Uses the known set of bot IDs (pool speakers + owner) for an O(M) delete rather
// than a full O(N*M) map scan with string prefix matching.
func (m *Service) clearAppliers(guildID snowflake.ID) {
	for _, botID := range m.poolSvc.GetIDs() {
		m.reconnectAppliers.Delete(botKey{guildID, botID})
	}
	m.reconnectAppliers.Delete(botKey{guildID, m.ownerBotID})
}

// buildSpeakerApplier returns a reconnectApplier for a speaker bot. It captures
// chOut and chCapture so the same mixer channels are reused after reconnect.
// A new VoiceReceiver is always created because disgo closes the old one on kick.
// The VoiceProvider is created fresh so two audioSender goroutines don't compete
// on the same channel (the old one is stopped; creating new is cleaner).
// FrameDropper is created inside the closure using the call-time ctx (the reconnect
// context) so metrics are never attached to the stale session-start span.
func (m *Service) buildSpeakerApplier(guildID, botID snowflake.ID, chOut <-chan []byte, withCapture bool, chCapture chan []byte, allowUser func(snowflake.ID) bool) reconnectApplier {
	isTest := m.test.IsTestBot(botID)
	return func(ctx context.Context, conn voice.Conn) {
		var provider voice.OpusFrameProvider
		if isTest {
			p, _ := opus.NewFileVoiceProvider(m.test.FileDCA)
			provider = p
		} else {
			onDrop := m.metrics.Session.FrameDropper(ctx, guildID, telemetry.DropPathProvider)
			provider = opus.NewVoiceProvider(chOut, onDrop)
		}
		conn.SetOpusFrameProvider(provider)
		if withCapture && chCapture != nil {
			conn.SetOpusFrameReceiver(opus.NewVoiceReceiver(chCapture, botID, allowUser))
		} else {
			conn.SetOpusFrameReceiver(opus.NewEmptyVoiceReceiver())
		}
	}
}

// buildOwnerApplier returns a reconnectApplier for the owner bot.
// chOut is nil when the session mode does not play back audio into the owner's channel.
// FrameDropper is created inside the closure using the call-time ctx (the reconnect
// context) so metrics are never attached to the stale session-start span.
func (m *Service) buildOwnerApplier(guildID snowflake.ID, chCapture chan []byte, chOut <-chan []byte, allowUser func(snowflake.ID) bool) reconnectApplier {
	hasOut := chOut != nil
	return func(ctx context.Context, conn voice.Conn) {
		var provider voice.OpusFrameProvider
		if hasOut {
			onDrop := m.metrics.Session.FrameDropper(ctx, guildID, telemetry.DropPathProvider)
			provider = opus.NewVoiceProvider(chOut, onDrop)
		} else {
			provider = opus.NewEmptyVoiceProvider()
		}
		conn.SetOpusFrameProvider(provider)
		if chCapture != nil {
			conn.SetOpusFrameReceiver(opus.NewVoiceReceiver(chCapture, m.ownerBotID, allowUser))
		} else {
			conn.SetOpusFrameReceiver(opus.NewEmptyVoiceReceiver())
		}
	}
}
