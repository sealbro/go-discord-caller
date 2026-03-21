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
	Speakers []*domain.Speaker
	RoleID   *snowflake.ID
	Session  *domain.VoiceSession
}

// String returns a human-readable summary of the status.
func (s *Status) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
		enabled := "✅"
		if !sp.Enabled {
			enabled = "❌"
		}
		bound := "unbound"
		if sp.BoundChannelID != nil {
			bound = fmt.Sprintf("<#%s>", sp.BoundChannelID)
		}
		sb.WriteString(fmt.Sprintf("- %s `%s` → %s %s\n", enabled, sp.Username, bound, sp.ID))
	}

	if s.RoleID != nil {
		sb.WriteString(fmt.Sprintf("\n**Capture Role:** <@&%s>\n", s.RoleID))
	} else {
		sb.WriteString("\n**Capture Role:** not set\n")
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
}

// NewService creates a new manager Service.
// pool is the ordered list of speaker bot tokens loaded from environment variables.
func NewService(st store.Store, spk *speaker.Service, pool []string) *Service {
	return &Service{store: st, speaker: spk, speakerPool: pool}
}

// HasAvailableToken reports whether the pool still has at least one token
// that has not been registered as a speaker in the given guild.
func (m *Service) HasAvailableToken(guildID snowflake.ID) bool {
	_, ok := m.nextAvailableToken(guildID)
	return ok
}

// nextAvailableToken returns the first pool token not yet used by a speaker in the guild.
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

// AddNextSpeaker picks the next unused pool token and registers it as a speaker in the guild.
func (m *Service) AddNextSpeaker(ctx context.Context, guildID snowflake.ID, displayName string) (*domain.Speaker, error) {
	token, ok := m.nextAvailableToken(guildID)
	if !ok {
		return nil, fmt.Errorf("no available speaker tokens left in the pool")
	}
	return m.AddSpeaker(ctx, guildID, token, displayName, nil)
}

// AddSpeaker registers a new speaker bot, persists it, and opens its gateway connection.
func (m *Service) AddSpeaker(ctx context.Context, guildID snowflake.ID, botToken, username string, allowedChannels []snowflake.ID) (*domain.Speaker, error) {
	sp := &domain.Speaker{
		ID:              snowflake.New(time.Now()),
		GuildID:         guildID,
		BotToken:        botToken,
		Username:        username,
		AllowedChannels: allowedChannels,
		Enabled:         true,
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

// RemoveSpeaker disconnects and removes a speaker from a guild.
func (m *Service) RemoveSpeaker(ctx context.Context, speakerID snowflake.ID) error {
	if _, ok := m.store.GetSpeaker(speakerID); !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}
	m.speaker.Disconnect(ctx, speakerID)
	m.store.RemoveSpeaker(speakerID)
	return nil
}

// ToggleSpeaker enables or disables a speaker without removing it.
func (m *Service) ToggleSpeaker(ctx context.Context, speakerID snowflake.ID, enabled bool) error {
	sp, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}

	sp.Enabled = enabled
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

// BindChannel binds a voice channel to a speaker (must be in the speaker's allowed list).
func (m *Service) BindChannel(speakerID, channelID snowflake.ID) error {
	sp, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}
	if !sp.HasChannelAccess(channelID) {
		return fmt.Errorf("channel <#%s> is not in the allowed list for speaker `%s`", channelID, sp.Username)
	}
	return m.store.BindChannel(speakerID, channelID)
}

// UnbindChannel removes the channel binding from a speaker.
func (m *Service) UnbindChannel(speakerID snowflake.ID) {
	m.store.UnbindChannel(speakerID)
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
		if !sp.Enabled || sp.BoundChannelID == nil {
			continue
		}
		if err := m.speaker.JoinChannel(ctx, sp.ID, guildID, *sp.BoundChannelID); err != nil {
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
		Speakers: m.store.ListSpeakers(guildID),
	}
	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		s.RoleID = &roleID
	}
	if session, ok := m.store.GetSession(guildID); ok {
		s.Session = session
	}
	return s
}
