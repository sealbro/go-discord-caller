package telemetry

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PoolMetrics tracks speaker pool connectivity and reconnect health.
type PoolMetrics struct {
	meter             metric.Meter // retained for RegisterObservers
	botInfo           metric.Int64ObservableGauge
	botsTotal         metric.Int64ObservableGauge
	botsConnected     metric.Int64ObservableGauge
	gatewayLatency    metric.Float64ObservableGauge
	botVerified       metric.Int64ObservableGauge
	botGuilds         metric.Int64ObservableGauge
	membersIntent     metric.Int64ObservableGauge
	verifyBlocked     metric.Int64ObservableGauge
	reconnectAttempts metric.Int64Counter
	reconnectFailures metric.Int64Counter
}

func (p *PoolMetrics) init(meter metric.Meter) (err error) {
	p.meter = meter
	if p.botInfo, err = meter.Int64ObservableGauge("gdc.discord.bot",
		metric.WithDescription("Info gauge for known bots; value is always 1. Labels: bot_id, bot_name."),
	); err != nil {
		return
	}
	if p.botsTotal, err = meter.Int64ObservableGauge("gdc.pool.bots.total",
		metric.WithDescription("Total speaker bots registered in the pool."),
	); err != nil {
		return
	}
	if p.botsConnected, err = meter.Int64ObservableGauge("gdc.pool.bots.connected",
		metric.WithDescription("Speaker bots with a healthy gateway connection."),
	); err != nil {
		return
	}
	if p.gatewayLatency, err = meter.Float64ObservableGauge("gdc.bot.gateway.latency",
		metric.WithDescription("Discord gateway WebSocket heartbeat RTT per bot. Zero until the first heartbeat ACK is received."),
		metric.WithUnit("ms"),
	); err != nil {
		return
	}
	if p.botVerified, err = meter.Int64ObservableGauge("gdc.bot.verified",
		metric.WithDescription("1 when Discord shows the bot as verified, 0 otherwise. An unverified bot cannot join more than 100 servers."),
	); err != nil {
		return
	}
	if p.botGuilds, err = meter.Int64ObservableGauge("gdc.bot.guilds",
		metric.WithDescription("Approximate number of servers the bot is in, as Discord counts it against the 100-server limit."),
	); err != nil {
		return
	}
	if p.membersIntent, err = meter.Int64ObservableGauge("gdc.bot.members_intent",
		metric.WithDescription("Always 1; the state label says whether the Guild Members intent is approved, limited (stops at 100 servers) or off."),
	); err != nil {
		return
	}
	if p.verifyBlocked, err = meter.Int64ObservableGauge("gdc.bot.verification_blocked",
		metric.WithDescription("1 when Discord has blocked verification because of unusual growth, 0 otherwise."),
	); err != nil {
		return
	}
	if p.reconnectAttempts, err = meter.Int64Counter("gdc.pool.reconnect.attempts.total",
		metric.WithDescription("Watchdog gateway reconnect attempts."),
	); err != nil {
		return
	}
	if p.reconnectFailures, err = meter.Int64Counter("gdc.pool.reconnect.failures.total",
		metric.WithDescription("Watchdog gateway reconnect failures."),
	); err != nil {
		return
	}
	return nil
}

// RegisterObservers registers cb as the observable callback for pool bot gauges.
func (p *PoolMetrics) RegisterObservers(cb metric.Callback) error {
	_, err := p.meter.RegisterCallback(cb, p.botInfo, p.botsTotal, p.botsConnected, p.gatewayLatency,
		p.botVerified, p.botGuilds, p.membersIntent, p.verifyBlocked)
	return err
}

// ObserveBotInfo emits a value of 1 for the given botID/botName pair via o.
// Call inside the callback registered with RegisterObservers.
func (p *PoolMetrics) ObserveBotInfo(o metric.Observer, botID, botName string) {
	o.ObserveInt64(p.botInfo, 1,
		metric.WithAttributes(
			attribute.String("bot_id", botID),
			attribute.String("bot_name", botName),
		),
	)
}

// ObservePoolBots reports current total and connected bot counts via o.
// Call inside the callback registered with RegisterObservers.
func (p *PoolMetrics) ObservePoolBots(o metric.Observer, total, connected int64) {
	o.ObserveInt64(p.botsTotal, total)
	o.ObserveInt64(p.botsConnected, connected)
}

// ObserveGatewayLatency reports the heartbeat RTT for one bot via o.
// Call inside the callback registered with RegisterObservers.
func (p *PoolMetrics) ObserveGatewayLatency(o metric.Observer, botID string, ms float64) {
	o.ObserveFloat64(p.gatewayLatency, ms,
		metric.WithAttributes(attribute.String("bot_id", botID)),
	)
}

// Guild Members intent states reported by gdc.bot.members_intent.
const (
	MembersIntentApproved = "approved"
	MembersIntentLimited  = "limited"
	MembersIntentOff      = "off"
)

// BotVerification is a bot's verification state as Discord last reported it.
type BotVerification struct {
	Verified      bool
	Guilds        *int // nil when Discord did not report a count
	MembersIntent string
	Blocked       bool
}

// ObserveBotVerification reports one bot's verification state via o.
// Call inside the callback registered with RegisterObservers.
func (p *PoolMetrics) ObserveBotVerification(o metric.Observer, botID string, v BotVerification) {
	bot := metric.WithAttributes(attribute.String("bot_id", botID))
	o.ObserveInt64(p.botVerified, boolToInt(v.Verified), bot)
	o.ObserveInt64(p.verifyBlocked, boolToInt(v.Blocked), bot)
	if v.Guilds != nil {
		o.ObserveInt64(p.botGuilds, int64(*v.Guilds), bot)
	}
	o.ObserveInt64(p.membersIntent, 1, metric.WithAttributes(
		attribute.String("bot_id", botID),
		attribute.String("state", v.MembersIntent),
	))
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// SeedReconnectSeries registers a zero data point for botID on both reconnect
// counters. OTel counters emit no Prometheus series at all until their first
// Add call, so a bot pool that has never needed a reconnect leaves the
// "Pool Reconnect Attempts / Failures" dashboard panels blank ("No data")
// rather than showing a flat 0 line. Call once per bot right after the pool
// is populated (before any real reconnect can occur) so the series exists
// from startup.
func (p *PoolMetrics) SeedReconnectSeries(ctx context.Context, botID snowflake.ID) {
	attrs := metric.WithAttributes(attribute.String("bot_id", botID.String()))
	p.reconnectAttempts.Add(ctx, 0, attrs)
	p.reconnectFailures.Add(ctx, 0, attrs)
}

// ReconnectAttempt records one watchdog reconnect attempt for botID.
func (p *PoolMetrics) ReconnectAttempt(ctx context.Context, botID snowflake.ID) {
	p.reconnectAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}

// ReconnectFailed records one failed watchdog reconnect attempt for botID.
func (p *PoolMetrics) ReconnectFailed(ctx context.Context, botID snowflake.ID) {
	p.reconnectFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}
