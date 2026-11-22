package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestVerificationOf(t *testing.T) {
	guilds := 87
	verifiedBot := &discord.User{PublicFlags: discord.UserFlagVerifiedBot}

	tests := []struct {
		name string
		app  discord.Application
		want telemetry.BotVerification
	}{
		{
			name: "unverified speaker without privileged intents",
			app:  discord.Application{Bot: &discord.User{}, ApproximateGuildCount: &guilds},
			want: telemetry.BotVerification{Guilds: &guilds, MembersIntent: telemetry.MembersIntentOff},
		},
		{
			name: "unverified owner with members intent",
			app:  discord.Application{Bot: &discord.User{}, Flags: discord.ApplicationFlagGatewayGuildMemberLimited},
			want: telemetry.BotVerification{MembersIntent: telemetry.MembersIntentLimited},
		},
		{
			name: "verified owner with approved members intent",
			app:  discord.Application{Bot: verifiedBot, Flags: discord.ApplicationFlagGatewayGuildMembers},
			want: telemetry.BotVerification{Verified: true, MembersIntent: telemetry.MembersIntentApproved},
		},
		{
			name: "verification blocked by unusual growth",
			app:  discord.Application{Bot: &discord.User{}, Flags: discord.ApplicationFlagVerificationPendingGuildLimit},
			want: telemetry.BotVerification{Blocked: true, MembersIntent: telemetry.MembersIntentOff},
		},
		{
			name: "no bot user in the response",
			app:  discord.Application{},
			want: telemetry.BotVerification{MembersIntent: telemetry.MembersIntentOff},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verificationOf(tt.app)
			if got.Verified != tt.want.Verified || got.MembersIntent != tt.want.MembersIntent ||
				got.Blocked != tt.want.Blocked || got.Guilds != tt.want.Guilds {
				t.Errorf("verificationOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// applicationRest answers GetCurrentApplication with app, or err when set.
type applicationRest struct {
	rest.Rest
	app discord.Application
	err error
}

func (r *applicationRest) GetCurrentApplication(...rest.RequestOpt) (*discord.Application, error) {
	if r.err != nil {
		return nil, r.err
	}
	app := r.app
	return &app, nil
}

// botSeries collects gdc.discord.bot keyed by its bot_id attribute.
func botSeries(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	points := map[string]metricdata.DataPoint[int64]{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "gdc.discord.bot" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				id, _ := dp.Attributes.Value("bot_id")
				if _, dup := points[id.AsString()]; dup {
					t.Fatalf("gdc.discord.bot: more than one series for bot %s", id.AsString())
				}
				points[id.AsString()] = dp
			}
		}
	}
	return points
}

func label(dp metricdata.DataPoint[int64], key attribute.Key) string {
	v, _ := dp.Attributes.Value(key)
	return v.Emit()
}

// Every bot — speakers and the owner — is polled with its own token, and a
// bot whose request fails keeps its last known state instead of vanishing
// from the 75/90-server alerts.
func TestPollVerificationReportsEveryBot(t *testing.T) {
	const speakerID, ownerID = snowflake.ID(10), snowflake.ID(20)
	guilds := 76

	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	s := NewService(&metrics.Pool, &metrics.Voice, nil, nil)
	speakerRest := &applicationRest{app: discord.Application{Bot: &discord.User{}, ApproximateGuildCount: &guilds}}
	ownerRest := &applicationRest{app: discord.Application{
		Bot:   &discord.User{PublicFlags: discord.UserFlagVerifiedBot},
		Flags: discord.ApplicationFlagGatewayGuildMembers,
	}}
	s.poolClients[speakerID] = &bot.Client{Rest: speakerRest, Caches: cache.New()}
	s.RegisterBot(ownerID, "owner", &bot.Client{Rest: ownerRest, Caches: cache.New()})
	s.StartMetrics()

	if before := botSeries(t, reader)[speakerID.String()]; before.Attributes.HasValue("verified") {
		t.Error("speaker carries a verified label before the first poll")
	}

	s.pollVerification(context.Background())

	bots := botSeries(t, reader)
	speaker, owner := bots[speakerID.String()], bots[ownerID.String()]
	if label(speaker, "verified") != "false" || label(owner, "verified") != "true" {
		t.Errorf("verified = speaker %q, owner %q; want false and true", label(speaker, "verified"), label(owner, "verified"))
	}
	if speaker.Value != 76 {
		t.Errorf("server count for the speaker = %d, want 76", speaker.Value)
	}
	if label(speaker, "members_intent") != telemetry.MembersIntentOff || label(owner, "members_intent") != telemetry.MembersIntentApproved {
		t.Errorf("members_intent = speaker %q, owner %q; want off and approved", label(speaker, "members_intent"), label(owner, "members_intent"))
	}
	if label(owner, "verification_blocked") != "false" {
		t.Errorf("verification_blocked for the owner = %q, want false", label(owner, "verification_blocked"))
	}

	speakerRest.err = errors.New("discord unavailable")
	s.pollVerification(context.Background())

	if got := botSeries(t, reader)[speakerID.String()]; got.Value != 76 || label(got, "verified") != "false" {
		t.Errorf("after a failed poll the speaker reports %d servers, verified %q; want the last known 76, false", got.Value, label(got, "verified"))
	}
}
