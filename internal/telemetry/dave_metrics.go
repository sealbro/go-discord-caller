package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DaveMetrics reports the outcome of every DAVE frame decryption.
//
// disgo logs a decrypt failure per packet with no SSRC or user ID attached, so
// the logs cannot distinguish one permanently undecryptable sender from every
// sender losing a fraction of frames — the two have unrelated causes. That
// distinction is the entire point of this instrument, and it is carried by the
// sender gauge: identities stay inside the process, and only the count of
// senders in each health bucket is exported. Three series per bot, whatever a
// channel's lifetime supply of distinct speakers.
type DaveMetrics struct {
	meter        metric.Meter // retained for RegisterObservers
	decryptTotal metric.Int64ObservableCounter
	senders      metric.Int64ObservableGauge
}

func (d *DaveMetrics) init(meter metric.Meter) (err error) {
	d.meter = meter
	if d.decryptTotal, err = meter.Int64ObservableCounter("gdc.dave.decrypt.total",
		metric.WithDescription("DAVE frame decryption outcomes, summed over senders. Labels: bot_user_id, outcome (success|passthrough|dropped|failure), reason (failure detail, else none)."),
	); err != nil {
		return
	}
	if d.senders, err = meter.Int64ObservableGauge("gdc.dave.decrypt.senders",
		metric.WithDescription("Distinct senders seen since the last collection, by decrypt health. Labels: bot_user_id, health (clean|partial|failing)."),
	); err != nil {
		return
	}
	return nil
}

// RegisterObservers registers cb as the observable callback for both decrypt
// instruments.
func (d *DaveMetrics) RegisterObservers(cb metric.Callback) error {
	_, err := d.meter.RegisterCallback(cb, d.decryptTotal, d.senders)
	return err
}

// ObserveDecrypt emits one decrypt counter sample. Call inside the callback
// registered with RegisterObservers.
func (d *DaveMetrics) ObserveDecrypt(o metric.Observer, botUserID, outcome, reason string, count int64) {
	o.ObserveInt64(d.decryptTotal, count,
		metric.WithAttributes(
			attribute.String("bot_user_id", botUserID),
			attribute.String("outcome", outcome),
			attribute.String("reason", reason),
		),
	)
}

// ObserveSenders emits the number of distinct senders in one health bucket.
// Call inside the callback registered with RegisterObservers.
func (d *DaveMetrics) ObserveSenders(o metric.Observer, botUserID, health string, senders int64) {
	o.ObserveInt64(d.senders, senders,
		metric.WithAttributes(
			attribute.String("bot_user_id", botUserID),
			attribute.String("health", health),
		),
	)
}
