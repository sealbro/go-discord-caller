package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Status is the view model returned by GetStatus — built from StatusStore + SessionStore.
type Status struct {
	GuildID        snowflake.ID
	Speakers       []*domain.Speaker             // all speakers registered for this guild
	Enabled        map[snowflake.ID]bool         // speakerID -> enabled
	BoundChannels  map[snowflake.ID]snowflake.ID // userID -> channelID
	RoleID         *snowflake.ID
	OwnerChannelID *snowflake.ID
	Session        *domain.VoiceSession
}

// String returns a human-readable summary of the status.
func (s *Status) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**Speakers (%d):**\n", len(s.Speakers)))
	for _, sp := range s.Speakers {
		enabled := "✅"
		if !s.Enabled[sp.ID] {
			enabled = "❌"
		}
		bound := "unbound"
		if chID, ok := s.BoundChannels[sp.ID]; ok {
			bound = fmt.Sprintf("<#%s>", chID)
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
		sb.WriteString(fmt.Sprintf("\n**Voice Raid:** 🔴 active (%d speakers joined)\n", len(s.Session.Speakers)))
	} else {
		sb.WriteString("\n**Voice Raid:** ⚫ inactive\n")
	}

	return sb.String()
}

// Service orchestrates speaker bots and voice raid sessions.
type Service struct {
	store       store.Store
	statusStore store.StatusStore
	sessions    store.SessionStore
	speakers    store.SpeakerStore
	speaker     *speaker.Service
	poolSvc     *pool.Service
	ownerClient *bot.Client
}

// NewService creates a new manager Service.
func NewService(st store.Store, statusStore store.StatusStore, sessions store.SessionStore, speakers store.SpeakerStore, spk *speaker.Service, poolSvc *pool.Service, client *bot.Client) *Service {
	return &Service{
		store:       st,
		statusStore: statusStore,
		sessions:    sessions,
		speakers:    speakers,
		speaker:     spk,
		poolSvc:     poolSvc,
		ownerClient: client,
	}
}

// guildStatus returns the stored GuildStatus for the guild, creating an empty one if absent.
func (m *Service) guildStatus(guildID snowflake.ID) *domain.GuildStatus {
	if st, ok := m.statusStore.GetStatus(guildID); ok {
		return st
	}
	return domain.NewGuildStatus(guildID)
}

// JoinChannel makes the owner bot join a voice channel.
func (m *Service) JoinChannel(ctx context.Context, guildID, channelID snowflake.ID) error {
	if m.ownerClient == nil {
		return fmt.Errorf("owner client not set")
	}
	conn := m.ownerClient.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return err
	}
	slog.Info("joined voice channel",
		slog.String("channelID", channelID.String()),
		slog.String("guildID", guildID.String()),
	)
	return nil
}

// LeaveChannel makes the owner bot leave its current voice channel in a guild.
func (m *Service) LeaveChannel(ctx context.Context, guildID snowflake.ID) {
	if m.ownerClient == nil {
		return
	}
	conn := m.ownerClient.VoiceManager.GetConn(guildID)
	if conn == nil {
		return
	}
	conn.Close(ctx)
	slog.Info("left voice channel", slog.String("guildID", guildID.String()))
}

// SeedExistingSpeakers checks every pool token against each supplied guild and
// registers any speaker bot that is already a member of that guild but is not
// yet tracked in the status store. Call this once on startup so that bots
// invited in a previous session are automatically re-registered.
func (m *Service) SeedExistingSpeakers(guildIDs []snowflake.ID) {
	for _, guildID := range guildIDs {
		st := m.guildStatus(guildID)

		for _, token := range m.poolSvc.GetTokens() {
			botUserID, ok := domain.BotUserID(token)
			if !ok {
				continue
			}
			if _, exists := st.Speakers[botUserID]; exists {
				continue // already registered for this guild
			}
			if !m.isGuildMember(guildID, botUserID) {
				continue
			}
			sp, err := m.AddSpeaker(guildID, token)
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
	_, ok := m.NextSpeakerID(guildID)
	return ok
}

// NextSpeakerID returns the Discord ApplicationID of the next pool speaker
// whose bot has NOT yet joined the guild.
func (m *Service) NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool) {
	st := m.guildStatus(guildID)

	for _, token := range m.poolSvc.GetTokens() {
		botUserID, ok := domain.BotUserID(token)
		if !ok {
			continue
		}
		if _, exists := st.Speakers[botUserID]; exists {
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
func (m *Service) ToggleSpeaker(speakerID, guildID snowflake.ID, enabled bool) error {
	st := m.guildStatus(guildID)
	if _, exists := st.Speakers[speakerID]; !exists {
		return fmt.Errorf("speaker %s is not registered in guild %s", speakerID, guildID)
	}
	st.Speakers[speakerID] = enabled
	m.statusStore.SetStatus(st)
	return nil
}

// BindChannel binds a voice channel to a user (speaker or owner) in a guild.
func (m *Service) BindChannel(userID, guildID, channelID snowflake.ID) {
	m.store.BindChannel(guildID, userID, channelID)
}

// UnbindChannel removes the channel binding for a user in a guild.
func (m *Service) UnbindChannel(userID, guildID snowflake.ID) {
	m.store.UnbindChannel(guildID, userID)
}

// GetBoundChannel returns the bound voice channel for a user in a guild.
func (m *Service) GetBoundChannel(userID, guildID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetBoundChannel(guildID, userID)
}

// BindOwnerChannel binds a voice channel to the owner bot for a guild.
func (m *Service) BindOwnerChannel(guildID, channelID snowflake.ID) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.BindChannel(guildID, ownerUser.ID, channelID)
}

// UnbindOwnerChannel removes the owner bot's channel binding for a guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.UnbindChannel(guildID, ownerUser.ID)
}

// GetOwnerChannel returns the bound voice channel for the owner bot in a guild.
func (m *Service) GetOwnerChannel(guildID snowflake.ID) (snowflake.ID, bool) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	return m.store.GetBoundChannel(guildID, ownerUser.ID)
}

// BindRole sets the Discord role whose members' voice will be captured in the guild.
func (m *Service) BindRole(guildID, roleID snowflake.ID) {
	m.store.BindRole(guildID, roleID)
	slog.Info("role bound",
		slog.String("guildID", guildID.String()),
		slog.String("roleID", roleID.String()),
	)
}

// ListSpeakers returns all Speaker objects registered for the guild.
func (m *Service) ListSpeakers(guildID snowflake.ID) []*domain.Speaker {
	st, ok := m.statusStore.GetStatus(guildID)
	if !ok {
		return nil
	}
	result := make([]*domain.Speaker, 0, len(st.Speakers))
	for spID := range st.Speakers {
		if sp, ok := m.speakers.GetSpeaker(spID); ok {
			result = append(result, sp)
		}
	}
	return result
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	if _, active := m.sessions.GetSession(guildID); active {
		return fmt.Errorf("a voice raid is already active in this server")
	}

	st, ok := m.statusStore.GetStatus(guildID)
	if !ok {
		return fmt.Errorf("no guild status found for guild %s", guildID)
	}

	ownerUser, _ := m.ownerClient.Caches.SelfUser()

	if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
		if err := m.JoinChannel(ctx, guildID, chID); err != nil {
			return fmt.Errorf("failed to join owner channel: %w", err)
		}
	}

	conn := m.ownerClient.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("no voice connection to join owner channel")
	}

	chIn := make(chan []byte, 10)
	receiver := opus.NewVoiceReceiver(chIn, ownerUser.ID)
	conn.SetOpusFrameReceiver(receiver)
	provider := opus.NewEmptyVoiceProvider()
	conn.SetOpusFrameProvider(provider)

	var outs []chan []byte
	newOut := func() chan []byte {
		ch := make(chan []byte, 10)
		outs = append(outs, ch)
		return ch
	}

	session := &domain.VoiceSession{GuildID: guildID, Active: true}

	for spID, enabled := range st.Speakers {
		if !enabled {
			continue
		}
		channelID, hasChannel := m.store.GetBoundChannel(guildID, spID)
		if !hasChannel {
			continue
		}
		sp, ok := m.speakers.GetSpeaker(spID)
		if !ok {
			continue
		}
		if err := m.speaker.JoinChannel(ctx, spID, guildID, channelID); err != nil {
			slog.Warn("speaker failed to join channel on raid start",
				slog.String("speakerID", spID.String()),
				slog.Any("err", err),
			)
			continue
		}
		session.Speakers = append(session.Speakers, sp)

		chOut := newOut()
		if err := m.speaker.Consume(ctx, spID, guildID, chOut); err != nil {
			slog.Error("failed to consume voice data", slog.String("speakerID", spID.String()), slog.Any("err", err))
		}
	}

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

	m.sessions.SetSession(session)
	slog.Info("voice raid started",
		slog.String("guildID", guildID.String()),
		slog.Int("activeSpeakers", len(session.Speakers)),
	)

	go func() {
		defer func() {
			provider.Close()
			receiver.Close()
			for _, out := range outs {
				close(out)
			}
			close(chIn)
			slog.Info("voice raid ended", slog.String("guildID", guildID.String()))
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-chIn:
				if !ok {
					return
				}
				for _, out := range outs {
					select {
					case out <- pkt:
					default:
					}
				}
			}
		}
	}()

	return nil
}

// StopVoiceRaid makes all active speakers leave their voice channels.
func (m *Service) StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	session, ok := m.sessions.GetSession(guildID)
	if !ok {
		return fmt.Errorf("no active voice raid in this server")
	}
	for _, sp := range session.Speakers {
		m.speaker.LeaveChannel(ctx, guildID, sp.ID)
	}
	m.LeaveChannel(ctx, guildID)
	m.sessions.DeleteSession(guildID)
	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// Shutdown stops every active voice raid and closes all speaker gateways.
func (m *Service) Shutdown(ctx context.Context) {
	slog.Info("shutting down manager service...")
	for _, session := range m.sessions.ListSessions() {
		if !session.Active {
			continue
		}
		if err := m.StopVoiceRaid(ctx, session.GuildID); err != nil {
			slog.Warn("shutdown: failed to stop voice raid",
				slog.String("guildID", session.GuildID.String()),
				slog.Any("err", err),
			)
		}
		m.LeaveChannel(ctx, session.GuildID)
	}
	m.poolSvc.Shutdown(ctx)
}

// GetStatus builds the Status view for a guild from StatusStore, Store, and SessionStore.
func (m *Service) GetStatus(guildID snowflake.ID) *Status {
	st := m.guildStatus(guildID)

	allSpeakers := make([]*domain.Speaker, 0, len(st.Speakers))
	boundChannels := make(map[snowflake.ID]snowflake.ID, len(st.Speakers))
	for spID := range st.Speakers {
		if sp, ok := m.speakers.GetSpeaker(spID); ok {
			allSpeakers = append(allSpeakers, sp)
		}
		if chID, ok := m.store.GetBoundChannel(guildID, spID); ok {
			boundChannels[spID] = chID
		}
	}

	s := &Status{
		GuildID:       guildID,
		Speakers:      allSpeakers,
		Enabled:       st.Speakers,
		BoundChannels: boundChannels,
	}

	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		s.RoleID = &roleID
	}
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	if chID, ok := m.store.GetBoundChannel(guildID, ownerUser.ID); ok {
		s.OwnerChannelID = &chID
	}
	if session, ok := m.sessions.GetSession(guildID); ok {
		s.Session = session
	}
	return s
}

// TrySeedMember checks whether a newly-joined guild member is an unregistered
// pool speaker bot and registers it if so.
func (m *Service) TrySeedMember(guildID, newUserID snowflake.ID) {
	for _, token := range m.poolSvc.GetTokens() {
		botUserID, ok := domain.BotUserID(token)
		if !ok || botUserID != newUserID {
			continue
		}
		sp, err := m.AddSpeaker(guildID, token)
		if err != nil {
			slog.Warn("member-join: failed to register speaker bot",
				slog.String("guildID", guildID.String()),
				slog.String("userID", botUserID.String()),
				slog.Any("err", err),
			)
			return
		}
		slog.Info("member-join: registered speaker bot",
			slog.String("username", sp.Username),
			slog.String("guildID", guildID.String()),
		)
		return
	}
}

// AddSpeaker registers a speaker bot for the given guild and marks it enabled in StatusStore.
func (m *Service) AddSpeaker(guildID snowflake.ID, botToken string) (*domain.Speaker, error) {
	sp, ok := m.speakers.GetSpeakerByToken(botToken)
	if !ok {
		user, ok := m.speaker.GetUserByToken(botToken)
		if !ok {
			return nil, fmt.Errorf("cannot resolve user for token")
		}
		sp = &domain.Speaker{
			ID:       user.ID,
			BotToken: botToken,
			Username: user.Username,
		}
		m.speaker.AssignClient(sp)
		m.speakers.AddSpeaker(sp)
	}

	st := m.guildStatus(guildID)
	st.Speakers[sp.ID] = true // enabled by default
	m.statusStore.SetStatus(st)

	slog.Info("speaker added",
		slog.String("username", sp.Username),
		slog.String("guildID", guildID.String()),
	)
	return sp, nil
}

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
	_, err := m.ownerClient.Rest.GetMember(guildID, userID)
	return err == nil
}
