package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/speaker"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// Service orchestrates speaker bots and voice raid sessions.
type Service struct {
	store       store.Store
	speaker     *speaker.Service
	ownerClient *bot.Client
	poolSvc     *pool.Service
}

// NewService creates a new manager Service.
// pool is the ordered list of speaker bot tokens loaded from environment variables.
func NewService(st store.Store, spk *speaker.Service, poolSvc *pool.Service, client *bot.Client) *Service {
	return &Service{store: st, speaker: spk, poolSvc: poolSvc, ownerClient: client}
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

		for _, token := range m.poolSvc.GetTokens() {
			if _, ok := usedTokens[token]; ok {
				continue // already tracked for this guild
			}

			botUserID, ok := domain.BotUserID(token)
			if !ok {
				slog.Warn("seed: invalid bot token format, skipping",
					slog.String("guildID", guildID.String()),
					slog.String("token", token),
				)
				continue
			}

			// Only seed when the bot is already a member of the guild.
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

// NextSpeakerID returns the Discord ApplicationID of the next pool
// speaker whose bot has NOT yet joined the guild.
// It iterates the ordered pool and skips every token that is either already
// registered in the store OR whose bot is already a guild member on Discord's
// side (e.g. invited in a previous session whose state was lost).
func (m *Service) NextSpeakerID(guildID snowflake.ID) (snowflake.ID, bool) {
	speakers := m.store.ListSpeakers(guildID)
	used := make(map[string]struct{}, len(speakers))
	for _, sp := range speakers {
		used[sp.BotToken] = struct{}{}
	}

	for _, token := range m.poolSvc.GetTokens() {
		if _, ok := used[token]; ok {
			continue // already registered in our store
		}
		botUserID, ok := domain.BotUserID(token)
		if !ok {
			slog.Warn("invalid bot token format, skipping")
			continue
		}
		// Skip if the bot is already a member of the guild on Discord's side.
		if m.isGuildMember(guildID, botUserID) {
			continue
		}
		return botUserID, true
	}
	return 0, false
}

// AddSpeaker registers a speaker bot for the given guild.
// If the bot token is already in the store (registered for another guild), the new guild is
// added to the existing speaker's Guilds map and the existing gateway connection is reused.
// Otherwise a new Speaker record is created and a gateway connection is opened.
func (m *Service) AddSpeaker(guildID snowflake.ID, botToken string) (*domain.Speaker, error) {
	membership := &domain.GuildMembership{
		Enabled: true,
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

	user, ok := m.speaker.GetUserByToken(botToken)
	if !ok {
		return nil, fmt.Errorf("cannot resolve user for token")
	}

	// No existing record — create one and open a new gateway connection.
	sp := &domain.Speaker{
		ID:       user.ID,
		BotToken: botToken,
		Username: user.Username,
		Guilds:   map[snowflake.ID]*domain.GuildMembership{guildID: membership},
	}

	m.store.AddSpeaker(sp)

	if err := m.speaker.AssignClient(sp); err != nil {
		m.store.RemoveSpeaker(sp.ID)
		return nil, fmt.Errorf("connect speaker: %w", err)
	}

	slog.Info("speaker added",
		slog.String("username", user.Username),
		slog.String("guildID", guildID.String()),
	)
	return sp, nil
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
		if err := m.speaker.AssignClient(sp); err != nil {
			return fmt.Errorf("reconnect speaker: %w", err)
		}
	}
	return nil
}

// BindChannel binds a voice channel to a speaker in a specific guild (must be in the speaker's allowed list).
func (m *Service) BindChannel(speakerID, guildID, channelID snowflake.ID) error {
	_, ok := m.store.GetSpeaker(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s not found", speakerID)
	}
	m.store.BindChannel(guildID, speakerID, channelID)
	return nil
}

// UnbindChannel removes the channel binding from a speaker in a specific guild.
func (m *Service) UnbindChannel(speakerID, guildID snowflake.ID) {
	m.store.UnbindChannel(guildID, speakerID)
}

// GetBoundChannel returns the bound voice channel for any user (speaker or owner) in a guild.
func (m *Service) GetBoundChannel(userID, guildID snowflake.ID) (snowflake.ID, bool) {
	return m.store.GetBoundChannel(guildID, userID)
}

// UnbindOwnerChannel removes the owner channel binding from a guild.
func (m *Service) UnbindOwnerChannel(guildID snowflake.ID) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.UnbindChannel(guildID, ownerUser.ID)
}

// BindOwnerChannel binds a voice channel to the owner of the guild.
func (m *Service) BindOwnerChannel(guildID, channelID snowflake.ID) {
	ownerUser, _ := m.ownerClient.Caches.SelfUser()
	m.store.BindChannel(guildID, ownerUser.ID, channelID)
}

// GetOwnerChannel returns the bound voice channel for the owner of the guild.
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

// ListSpeakers returns all speakers registered in the guild.
func (m *Service) ListSpeakers(guildID snowflake.ID) []*domain.Speaker {
	return m.store.ListSpeakers(guildID)
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	if _, active := m.store.GetSession(guildID); active {
		return fmt.Errorf("a voice raid is already active in this server")
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

	speakers := m.store.ListSpeakers(guildID)
	session := &domain.VoiceSession{GuildID: guildID, Active: true}

	for _, sp := range speakers {
		membership, ok := sp.Guilds[guildID]
		channelID, hasChannel := m.store.GetBoundChannel(guildID, sp.ID)
		if !ok || !membership.Enabled || !hasChannel {
			continue
		}
		if err := m.speaker.JoinChannel(ctx, sp.ID, guildID, channelID); err != nil {
			slog.Warn("speaker failed to join channel on raid start",
				slog.String("speakerID", sp.ID.String()),
				slog.Any("err", err),
			)
			continue
		}
		session.Speakers = append(session.Speakers, sp)

		chOut := newOut()
		err := m.speaker.Consume(ctx, sp.ID, guildID, chOut)
		if err != nil {
			slog.Error("failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
			continue
		}
	}

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}

	m.store.SetSession(session)
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
	session, ok := m.store.GetSession(guildID)
	if !ok {
		return fmt.Errorf("no active voice raid in this server")
	}

	for _, sp := range session.Speakers {
		m.speaker.LeaveChannel(ctx, sp.ID, guildID)
	}

	m.LeaveChannel(ctx, guildID)

	m.store.DeleteSession(guildID)
	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// Shutdown stops every active voice raid, closes all speaker gateways (assigned
// and pool), and leaves every owner voice channel. Call this before closing the
// owner bot client.
func (m *Service) Shutdown(ctx context.Context) {
	slog.Info("shutting down manager service...")

	// Stop every active raid: cancels relay goroutines and leaves speaker channels.
	for _, session := range m.store.ListSessions() {
		if !session.Active {
			continue
		}
		if err := m.StopVoiceRaid(ctx, session.GuildID); err != nil {
			slog.Warn("shutdown: failed to stop voice raid",
				slog.String("guildID", session.GuildID.String()),
				slog.Any("err", err),
			)
		}
		// Leave the owner bot's voice channel for this guild.
		m.LeaveChannel(ctx, session.GuildID)
	}

	// Shut down all speaker gateways (assigned + pool).
	m.poolSvc.Shutdown(ctx)
}

// GetStatus returns the current speaker and session state for a guild.
func (m *Service) GetStatus(guildID snowflake.ID) *Status {
	speakers := m.store.ListSpeakers(guildID)
	boundChannels := make(map[snowflake.ID]snowflake.ID, len(speakers))
	for _, sp := range speakers {
		if chID, ok := m.store.GetBoundChannel(guildID, sp.ID); ok {
			boundChannels[sp.ID] = chID
		}
	}

	s := &Status{
		GuildID:       guildID,
		Speakers:      speakers,
		BoundChannels: boundChannels,
	}
	if roleID, ok := m.store.GetBoundRole(guildID); ok {
		s.RoleID = &roleID
	}
	if chID, ok := m.GetOwnerChannel(guildID); ok {
		s.OwnerChannelID = &chID
	}
	if session, ok := m.store.GetSession(guildID); ok {
		s.Session = session
	}
	return s
}

// TrySeedMember checks whether a newly-joined guild member is an unregistered
// pool speaker bot. If it is, the bot is registered via AddSpeaker exactly as
// SeedExistingSpeakers does on startup. Safe to call concurrently.
func (m *Service) TrySeedMember(guildID, newUserID snowflake.ID) {
	for _, token := range m.poolSvc.GetTokens() {
		botUserID, ok := domain.BotUserID(token)
		if !ok {
			continue // invalid token format
		}

		if botUserID != newUserID {
			continue // not the bot that just joined
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

func (m *Service) isGuildMember(guildID, userID snowflake.ID) bool {
	_, err := m.ownerClient.Rest.GetMember(guildID, userID)
	return err == nil
}
