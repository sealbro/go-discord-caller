package manager

import (
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// VoiceHost carries the voice connections of the owner and speaker bots. The
// service joins and leaves voice only through it.
type VoiceHost interface {
	// VoiceManager returns the voice manager for botID's connections, or false
	// when the bot is unknown or its gateway is not connected.
	VoiceManager(botID snowflake.ID) (voice.Manager, bool)
}

// localVoiceHost runs voice on the bots' own disgo clients in this process.
type localVoiceHost struct {
	ownerID snowflake.ID
	owner   *bot.Client
	pool    pool.PoolService
}

func (h localVoiceHost) VoiceManager(botID snowflake.ID) (voice.Manager, bool) {
	if botID == h.ownerID {
		return h.owner.VoiceManager, true
	}
	client, ok := h.pool.GetClientByID(botID)
	if !ok {
		return nil, false
	}
	return client.VoiceManager, true
}

func (m *Service) voiceHost() VoiceHost {
	if m.voices != nil {
		return m.voices
	}
	return localVoiceHost{ownerID: m.ownerBotID, owner: m.ownerClient, pool: m.poolSvc}
}
