package manager

import (
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
)

// liveBoundChannel returns botID's bound channel in guildID, provided the
// channel still exists. A join to a deleted channel never completes and costs
// the whole join budget, so callers treat a missing channel as unbound.
func (m *Service) liveBoundChannel(guildID, botID snowflake.ID) (snowflake.ID, bool) {
	channelID, ok := m.store.GetBoundChannel(guildID, botID)
	if !ok {
		return 0, false
	}
	if _, exists := m.ownerClient.Caches.Channel(channelID); !exists {
		return 0, false
	}
	return channelID, true
}

// SpeakersWithMissingChannel returns the enabled speakers in guildID whose bound
// channel no longer exists. A raid skips them instead of waiting out a join.
func (m *Service) SpeakersWithMissingChannel(guildID snowflake.ID) []ChannelAccessWarning {
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return nil
	}
	var missing []ChannelAccessWarning
	for _, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		channelID, bound := m.store.GetBoundChannel(guildID, sp.ID)
		if !bound {
			continue
		}
		if _, exists := m.ownerClient.Caches.Channel(channelID); !exists {
			missing = append(missing, ChannelAccessWarning{BotID: sp.ID, ChannelID: channelID})
		}
	}
	return missing
}

// ChannelDeleted unbinds every bot in guildID that was bound to channelID.
func (m *Service) ChannelDeleted(guildID, channelID snowflake.ID) {
	m.unbindChannels(guildID, func(boundID snowflake.ID) bool { return boundID == channelID })
}

// unbindChannels removes the binding of the owner bot and every pool bot in
// guildID whose bound channel is gone.
func (m *Service) unbindChannels(guildID snowflake.ID, gone func(channelID snowflake.ID) bool) {
	botIDs := append([]snowflake.ID{m.ownerBotID}, m.poolSvc.GetIDs()...)
	for _, botID := range botIDs {
		channelID, ok := m.store.GetBoundChannel(guildID, botID)
		if !ok || !gone(channelID) {
			continue
		}
		m.store.UnbindChannel(guildID, botID)
		slog.Info("unbound bot from deleted channel",
			slog.String("botUserID", botID.String()),
			slog.String("guildID", guildID.String()),
			slog.String("channelID", channelID.String()),
		)
	}
}
