package pool

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// verificationOf reads a bot's verification state from its own application.
func verificationOf(app discord.Application) telemetry.BotVerification {
	v := telemetry.BotVerification{
		Verified:      app.Bot != nil && app.Bot.PublicFlags.Has(discord.UserFlagVerifiedBot),
		Guilds:        app.ApproximateGuildCount,
		MembersIntent: telemetry.MembersIntentOff,
		Blocked:       app.Flags.Has(discord.ApplicationFlagVerificationPendingGuildLimit),
	}
	switch {
	case app.Flags.Has(discord.ApplicationFlagGatewayGuildMembers):
		v.MembersIntent = telemetry.MembersIntentApproved
	case app.Flags.Has(discord.ApplicationFlagGatewayGuildMemberLimited):
		v.MembersIntent = telemetry.MembersIntentLimited
	}
	return v
}

// StartVerificationPoll fetches every bot's verification state now and then
// every interval. Call after the owner bot is registered so it is included.
func (s *Service) StartVerificationPoll(ctx context.Context, interval time.Duration) {
	go func() {
		s.pollVerification(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollVerification(ctx)
			}
		}
	}()
}

// pollVerification asks Discord for each bot's application with that bot's own
// token. A failed request keeps the last known state rather than dropping it.
func (s *Service) pollVerification(ctx context.Context) {
	for id, client := range s.allClients() {
		app, err := client.Rest.GetCurrentApplication(rest.WithCtx(ctx))
		if err != nil {
			slog.WarnContext(ctx, "pool: failed to fetch bot verification state",
				slog.String("botUserID", id.String()), slog.Any("err", err))
			continue
		}
		v := verificationOf(*app)
		s.mu.Lock()
		s.verification[id] = v
		s.mu.Unlock()
	}
}

func (s *Service) allClients() map[snowflake.ID]*bot.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make(map[snowflake.ID]*bot.Client, len(s.poolClients)+len(s.extraBots))
	for id, c := range s.poolClients {
		if c != nil {
			clients[id] = c
		}
	}
	for id, eb := range s.extraBots {
		if eb.client != nil {
			clients[id] = eb.client
		}
	}
	return clients
}
