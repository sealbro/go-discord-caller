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
	bot               metric.Int64ObservableGauge
	botsTotal         metric.Int64ObservableGauge
	botsConnected     metric.Int64ObservableGauge
	gatewayLatency    metric.Float64ObservableGauge
	reconnectAttempts metric.Int64Counter
	reconnectFailures metric.Int64Counter
	gatewayReady      metric.Int64Counter
}

func (p *PoolMetrics) init(meter metric.Meter) (err error) {
	p.meter = meter
	if p.bot, err = meter.Int64ObservableGauge("gdc.discord.bot",
		metric.WithDescription("One series per bot; value is the approximate number of servers it is in, as Discord counts it against the 100-server limit, or 0 until Discord reports it. Labels: bot_id, bot_name, and once Discord has reported them: verified, verification_blocked (true|false) and members_intent (approved|limited|off)."),
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
		metric.WithDescription("Discord gateway WebSocket heartbeat RTT per bot. Zero until the first heartbeat ACK; absent while a later heartbeat is awaiting its ACK."),
		metric.WithUnit("ms"),
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
	if p.gatewayReady, err = meter.Int64Counter("gdc.gateway.ready.total",
		metric.WithDescription("Gateway READY events received. Each one follows an IDENTIFY, which Discord caps per day; a steady climb means a reconnect loop."),
	); err != nil {
		return
	}
	return nil
}

// RegisterObservers registers cb as the observable callback for pool bot gauges.
func (p *PoolMetrics) RegisterObservers(cb metric.Callback) error {
	_, err := p.meter.RegisterCallback(cb, p.bot, p.botsTotal, p.botsConnected, p.gatewayLatency)
	return err
}

// ObserveBot emits one bot's series via o. v is nil until Discord has
// reported the bot's verification state.
// Call inside the callback registered with RegisterObservers.
func (p *PoolMetrics) ObserveBot(o metric.Observer, botID, botName string, v *BotVerification) {
	attrs := []attribute.KeyValue{
		attribute.String("bot_id", botID),
		attribute.String("bot_name", botName),
	}
	var guilds int64
	if v != nil {
		attrs = append(attrs,
			attribute.Bool("verified", v.Verified),
			attribute.Bool("verification_blocked", v.Blocked),
			attribute.String("members_intent", v.MembersIntent),
		)
		if v.Guilds != nil {
			guilds = int64(*v.Guilds)
		}
	}
	o.ObserveInt64(p.bot, guilds, metric.WithAttributes(attrs...))
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

// Guild Members intent states, reported as the members_intent label of gdc.discord.bot.
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

// GatewayReady records one READY event for botID, seeding the series at zero
// first so a bot that never re-identifies still shows a flat line.
func (p *PoolMetrics) GatewayReady(ctx context.Context, botID snowflake.ID) {
	p.gatewayReady.Add(ctx, 1, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}

// SeedGatewayReady creates the READY series for botID at zero.
func (p *PoolMetrics) SeedGatewayReady(ctx context.Context, botID snowflake.ID) {
	p.gatewayReady.Add(ctx, 0, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}
