package manager

import (
	"log/slog"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/dave"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// voiceRequest is something the voice side of a raid asks the service to do.
// The service decides from its own state whether to do it; nothing in a
// request is taken on trust beyond naming what it is about.
type voiceRequest interface {
	isVoiceRequest()
}

// encryptStallRequest asks for a bot whose outbound encryption has stalled to
// rejoin its channel. See OnEncryptStall.
type encryptStallRequest struct {
	GuildID, BotID snowflake.ID
	Trace          dave.Trace
}

func (encryptStallRequest) isVoiceRequest() {}

// handleVoiceRequest carries out req when the guild's raid allows it, and
// reports whether it did.
func (m *Service) handleVoiceRequest(req voiceRequest) bool {
	switch r := req.(type) {
	case encryptStallRequest:
		return m.handleEncryptStall(r)
	default:
		return false
	}
}

func (m *Service) handleEncryptStall(r encryptStallRequest) bool {
	if !m.botInRaid(r.GuildID, r.BotID) {
		slog.Debug("voice: encrypt stall for a bot outside the guild's raid, ignored",
			slog.String("guildID", r.GuildID.String()),
			slog.String("botUserID", r.BotID.String()),
		)
		return false
	}
	if !m.reconnect.stallRejoinDue(r.GuildID, r.BotID, time.Now()) {
		slog.Debug("voice: outbound encryption still stalled, rejoin backing off",
			slog.String("guildID", r.GuildID.String()),
			slog.String("botUserID", r.BotID.String()),
		)
		return false
	}
	slog.Warn("voice: outbound encryption stalled, rejoining",
		slog.String("guildID", r.GuildID.String()),
		slog.String("botUserID", r.BotID.String()),
		slog.Any("daveTrace", r.Trace),
	)
	m.rejoinStalled(r.GuildID, r.BotID)
	return true
}

// botInRaid reports whether botID is the owner or a speaker of guildID's
// running raid.
func (m *Service) botInRaid(guildID, botID snowflake.ID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	if st == nil || st.Session == nil {
		return false
	}
	if botID == m.ownerBotID {
		return true
	}
	return slices.ContainsFunc(st.Session.Speakers, func(sp guild.Speaker) bool { return sp.ID == botID })
}
