package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Status holds the current state of speakers and bindings in a guild.
type Status struct {
	GuildID        snowflake.ID
	Speakers       []*domain.Speaker
	RoleID         *snowflake.ID
	OwnerChannelID *snowflake.ID
	Session        *domain.VoiceSession
}

// String returns a human-readable summary of the status.
func (s *Status) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
		membership, ok := sp.Guilds[s.GuildID]
		if !ok {
			continue
		}
		enabled := "✅"
		if !membership.Enabled {
			enabled = "❌"
		}
		bound := "unbound"
		if membership.BoundChannelID != nil {
			bound = fmt.Sprintf("<#%s>", membership.BoundChannelID)
		}
		sb.WriteString(fmt.Sprintf("- %s <@%s> → %s\n", enabled, sp.ID, bound))
	}

	if s.RoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Capture Role:** <@&%s>\n", s.RoleID))
	} else {
		sb.WriteString("\n**Capture Role:** not set\n")
	}

	if s.OwnerChannelID != nil {
		sb.WriteString(fmt.Sprintf("\n**Owner Bot Channel:** <#%s>\n", s.OwnerChannelID))
	} else {
		sb.WriteString("\n**Owner Bot Channel:** not set\n")
	}

	if s.Session != nil && s.Session.Active {
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.SpeakerIDs)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}

// Service orchestrates speaker bots and voice raid sessions.
type Service struct {
	store       store.Store
	speaker     *speaker.Service
	speakerPool []string // ordered list of bot tokens available for registration
	isMemberFn  func(guildID, userID snowflake.ID) bool
}

// NewService creates a new manager Service.
// pool is the ordered list of speaker bot tokens loaded from environment variables.
func NewService(st store.Store, spk *speaker.Service, pool []string) *Service {
	return &Service{store: st, speaker: spk, speakerPool: pool}
}

// SetMemberChecker supplies a live Discord membership check used by
// NextSpeakerClientID to skip bots that are already in the guild.
// Call this once after the owner bot client has been created.
func (m *Service) SetMemberChecker(fn func(guildID, userID snowflake.ID) bool) {
	m.isMemberFn = fn
}

// SeedExistingSpeakers checks every pool token against each supplied guild and
// registers any speaker bot that is already a member of that guild but is not
// yet tracked in the store.  Call this once on startup (e.g. from the Ready
// event handler) so that bots invited in a previous session are automatically
// re-registered without manual /add-speaker commands.
func (m *Service) SeedExistingSpeakers(ctx context.Context, guildIDs []snowflake.ID) {
	for _, guildID := range guildIDs {
		// Build the set of tokens already registered for this guild.
		registered := m.store.ListSpeakers(guildID)
		usedTokens := make(map[string]struct{}, len(registered))
		for _, sp := range registered {
			usedTokens[sp.BotToken] = struct{}{}
		}

		for _, token := range m.speakerPool {
			if _, ok := usedTokens[token]; ok {
				continue // already tracked for this guild
			}

			clientID, ok := m.speaker.NextPoolClientID(token)
			if !ok {
				continue // cannot determine ApplicationID for this token
			}

			// Only seed when the bot is already a member of the guild.
			if m.isMemberFn == nil || !m.isMemberFn(guildID, clientID) {
				continue
			}

			username, _ := m.speaker.PoolClientUsername(token)

			sp, err := m.AddSpeaker(ctx, guildID, token, username, nil)
			if err != nil {
				slog.Warn("seed: failed to register existing speaker bot",
					slog.String("guildID", guildID.String()),
					slog.Any("err", err),
				)
				continue
			}
			slog.Info("seed: registered existing speaker bot",
				slog.String("username", sp.Username),
				slog.String("guildID", guildID.String()),
			)
		}
	}
}

// HasAvailableToken reports whether the pool has at least one speaker bot
// that has not yet been added to the given guild.
func (m *Service) HasAvailableToken(guildID snowflake.ID) bool {
	_, ok := m.NextSpeakerClientID(guildID)
	return ok
}

// nextAvailableToken returns the first pool token whose bot has not yet been
// registered as a speaker in the store for this guild.
// Used by AddNextSpeaker which only needs the store-based check.
func (m *Service) nextAvailableToken(guildID snowflake.ID) (string, bool) {
	speakers := m.store.ListSpeakers(guildID)
	used := make(map[string]struct{}, len(speakers))
	for _, sp := range speakers {
		used[sp.BotToken] = struct{}{}
	}
	for _, token := range m.speakerPool {
		if _, ok := used[token]; !ok {
			return token, true
		}
	}
	return "", false
}

// NextSpeakerClientID returns the Discord ApplicationID of the next pool
// speaker whose bot has NOT yet joined the guild.
// It iterates the ordered pool and skips every token that is either already
// registered in the store OR whose bot is already a guild member on Discord's
// side (e.g. invited in a previous session whose state was lost).
func (m *Service) NextSpeakerClientID(guildID snowflake.ID) (snowflake.ID, bool) {
	speakers := m.store.ListSpeakers(guildID)
	used := make(map[string]struct{}, len(speakers))
	for _, sp := range speakers {
		used[sp.BotToken] = struct{}{}
	}

	for _, token := range m.speakerPool {
		if _, ok := used[token]; ok {
			continue // already registered in our store
		}
		clientID, ok := m.speaker.NextPoolClientID(token)
		if !ok {
			continue // cannot determine ApplicationID for this token
		}
		// Skip if the bot is already a member of the guild on Discord's side.
		if m.isMemberFn != nil && m.isMemberFn(guildID, clientID) {
			continue
		}
		return clientID, true
	}
	return 0, false
}

// AddNextSpeaker picks the next pool token not yet registered in the store and
// registers it as a speaker in the guild.  It uses the store-only check so that
// a bot that was just invited (already in the guild) is registered correctly.
func (m *Service) AddNextSpeaker(ctx context.Context, guildID snowflake.ID, displayName string) (*domain.Speaker, error) {
	token, ok := m.nextAvailableToken(guildID)
	if !ok {
		return nil, fmt.Errorf("no available speaker tokens left in the pool")
	}
	return m.AddSpeaker(ctx, guildID, token, displayName, nil)
}

// AddSpeaker registers a speaker bot for the given guild.
// If the bot token is already in the store (registered for another guild), the new guild is
// added to the existing speaker's Guilds map and the existing gateway connection is reused.
// Otherwise a new Speaker record is created and a gateway connection is opened.
func (m *Service) AddSpeaker(ctx context.Context, guildID snowflake.ID, botToken, username string, allowedChannels []snowflake.ID) (*domain.Speaker, error) {
	membership := &domain.GuildMembership{
		AllowedChannels: allowedChannels,
		Enabled:         true,
	}

	// Reuse an existing speaker record for this bot token.
	if existing, ok := m.store.GetSpeakerByToken(botToken); ok {
		existing.Guilds[guildID] = membership
		if err := m.store.UpdateSpeaker(existing); err != nil {
			return nil, fmt.Errorf("update speaker with new guild: %w", err)
		}
		slog.Info("speaker added to guild (reusing existing connection)",
			slog.String("username", existing.Username),
			slog.String("speakerID", existing.ID.String()),
			slog.String("guildID", guildID.String()),
		)
		return existing, nil
	}

	// No existing record — create one and open a new gateway connection.
	sp := &domain.Speaker{
		ID:       snowflake.New(time.Now()),
		BotToken: botToken,
		Username: username,
		Guilds:   map[snowflake.ID]*domain.GuildMembership{guildID: membership},
	}

	m.store.AddSpeaker(sp)

	if err := m.speaker.Connect(ctx, sp); err != nil {
		m.store.RemoveSpeaker(sp.ID)
		return nil, fmt.Errorf("connect speaker: %w", err)
	}

	slog.Info("speaker added",
		slog.String("username", username),
		slog.String("guildID", guildID.String()),
	)
	return sp, nil
}

// RemoveSpeaker removes a speaker from the given guild.
// The gateway connection and store record are only torn down when the speaker
// has no remaining guild memberships.
func (m *Service) RemoveSpeaker(ctx context.Context, speakerID, guildID snowflake.ID) error {
	sp, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}

	if _, ok := sp.Guilds[guildID]; !ok {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
	}

	delete(sp.Guilds, guildID)

	if len(sp.Guilds) == 0 {
		// Last guild — disconnect and purge entirely.
		m.speaker.Disconnect(ctx, speakerID)
		m.store.RemoveSpeaker(speakerID)
		slog.Info("speaker removed (no remaining guilds)",
			slog.String("speakerID", speakerID.String()),
		)
	} else {
		// Still active in other guilds — just persist the updated map.
		if err := m.store.UpdateSpeaker(sp); err != nil {
			return err
		}
		slog.Info("speaker removed from guild",
			slog.String("speakerID", speakerID.String()),
			slog.String("guildID", guildID.String()),
		)
	}
	return nil
}

// ToggleSpeaker enables or disables a speaker within a specific guild without removing it.
func (m *Service) ToggleSpeaker(ctx context.Context, speakerID, guildID snowflake.ID, enabled bool) error {
	sp, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}

	membership, ok := sp.Guilds[guildID]
	if !ok {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
	}

	membership.Enabled = enabled
	if err := m.store.UpdateSpeaker(sp); err != nil {
		return err
	}

	if !enabled {
		m.speaker.Disconnect(ctx, speakerID)
	} else {
		if err := m.speaker.Connect(ctx, sp); err != nil {
			return fmt.Errorf("reconnect speaker: %w", err)
		}
	}
	return nil
}

// BindChannel binds a voice channel to a speaker in a specific guild (must be in the speaker's allowed list).
func (m *Service) BindChannel(speakerID, guildID, channelID snowflake.ID) error {
	sp, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}
	if !sp.HasChannelAccess(guildID, channelID) {
		return fmt.Errorf("channel <#%s> is not in the allowed list for speaker `%s`", channelID, sp.Username)
	}
	return m.store.BindChannel(speakerID, guildID, channelID)
}

// UnbindChannel removes the channel binding from a speaker in a specific guild.
func (m *Service) UnbindChannel(speakerID, guildID snowflake.ID) {
	m.store.UnbindChannel(speakerID, guildID)
}

// BindOwnerChannel sets the voice channel the owner bot will join in the guild.
func (m *Service) BindOwnerChannel(guildID, channelID snowflake.ID) {
	m.store.BindOwnerChannel(guildID, channelID)
	slog.Info("owner channel bound",
		slog.String("guildID", guildID.String()),
		slog.String("channelID", channelID.String()),
	)
}

// UnbindOwnerChannel removes the owner bot's voice channel binding for the guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	m.store.UnbindOwnerChannel(guildID)
}

// GetOwnerChannel returns the channel the owner bot is configured to join in the guild.
func (m *Service) GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetOwnerChannel(guildID)
}

// BindRole sets the Discord role whose members' voice will be captured in the guild.
func (m *Service) BindRole(guildID, roleID snowflake.ID) {
	m.store.BindRole(guildID, roleID)
	slog.Info("role bound",
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
}

// UnbindRole removes the role binding from a guild.
func (m *Service) UnbindRole(guildID snowflake.ID) {
	m.store.UnbindRole(guildID)
}

// ListSpeakers returns all speakers registered in the guild.
func (m *Service) ListSpeakers(guildID snowflake.ID) []*domain.Speaker {
	return m.store.ListSpeakers(guildID)
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	if _, active := m.store.GetSession(guildID); active {
		return fmt.Errorf("a voice raid is already active in this server")
	}

	speakers := m.store.ListSpeakers(guildID)
	session := &domain.VoiceSession{GuildID: guildID, Active: true}

	for _, sp := range speakers {
		membership, ok := sp.Guilds[guildID]
		if !ok || !membership.Enabled || membership.BoundChannelID == nil {
			continue
		}
		if err := m.speaker.JoinChannel(ctx, sp.ID, guildID, *membership.BoundChannelID); err != nil {
			slog.Warn("speaker failed to join channel on raid start",
				slog.String("speakerID", sp.ID.String()),
				slog.Any("err", err),
			)
			continue
		}
		session.SpeakerIDs = append(session.SpeakerIDs, sp.ID)
	}

	m.store.SetSession(session)
	slog.Info("voice raid started",
		slog.String("guildID", guildID.String()),
		slog.Int("activeSpeakers", len(session.SpeakerIDs)),
	)
	return nil
}

// StopVoiceRaid makes all active speakers leave their voice channels.
func (m *Service) StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	session, ok := m.store.GetSession(guildID)
	if !ok {
		return fmt.Errorf("no active voice raid in this server")
	}

	for _, speakerID := range session.SpeakerIDs {
		m.speaker.LeaveChannel(ctx, speakerID, guildID)
	}

	m.store.DeleteSession(guildID)
	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// GetStatus returns the current speaker and session state for a guild.
func (m *Service) GetStatus(guildID snowflake.ID) *Status {
	s := &Status{
		GuildID:  guildID,
		Speakers: m.store.ListSpeakers(guildID),
	}
	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		s.RoleID = &roleID
	}
	if chID, ok := m.store.GetOwnerChannel(guildID); ok {
		s.OwnerChannelID = &chID
	}
	if session, ok := m.store.GetSession(guildID); ok {
		s.Session = session
	}
	return s
}
