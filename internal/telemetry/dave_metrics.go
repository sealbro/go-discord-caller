package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DaveMetrics reports the outcome of every DAVE frame decryption, split by the
// user whose audio it was.
//
// disgo logs a decrypt failure per packet with no SSRC or user ID attached, so
// the logs cannot distinguish one permanently undecryptable sender from every
// sender losing a fraction of frames — the two have unrelated causes. The
// user_id label is the entire point of this instrument; it is bounded by the
// number of people talking in a channel, not by session or frame count.
type DaveMetrics struct {
	meter        metric.Meter // retained for RegisterObservers
	decryptTotal metric.Int64ObservableCounter
}

func (d *DaveMetrics) init(meter metric.Meter) (err error) {
	d.meter = meter
	if d.decryptTotal, err = meter.Int64ObservableCounter("gdc.dave.decrypt.total",
		metric.WithDescription("DAVE frame decryption outcomes. Labels: bot_user_id, user_id, outcome (success|unknown_user|failure), reason (failure detail, else none)."),
	); err != nil {
		return
	}
	return nil
}

// RegisterObservers registers cb as the observable callback for decrypt counts.
func (d *DaveMetrics) RegisterObservers(cb metric.Callback) error {
	_, err := d.meter.RegisterCallback(cb, d.decryptTotal)
	return err
}

// ObserveDecrypt emits one decrypt counter sample. Call inside the callback
// registered with RegisterObservers.
func (d *DaveMetrics) ObserveDecrypt(o metric.Observer, botUserID, userID, outcome, reason string, count int64) {
	o.ObserveInt64(d.decryptTotal, count,
		metric.WithAttributes(
			attribute.String("bot_user_id", botUserID),
			attribute.String("user_id", userID),
			attribute.String("outcome", outcome),
			attribute.String("reason", reason),
		),
	)
}
