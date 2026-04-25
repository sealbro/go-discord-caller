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
	_, err := p.meter.RegisterCallback(cb, p.botInfo, p.botsTotal, p.botsConnected, p.gatewayLatency)
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

// ReconnectAttempt records one watchdog reconnect attempt for botID.
func (p *PoolMetrics) ReconnectAttempt(ctx context.Context, botID snowflake.ID) {
	p.reconnectAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}

// ReconnectFailed records one failed watchdog reconnect attempt for botID.
func (p *PoolMetrics) ReconnectFailed(ctx context.Context, botID snowflake.ID) {
	p.reconnectFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("bot_id", botID.String())))
}
