package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DaveObservation is one aggregated reading of the DAVE (E2EE voice) session
// counters, summed across every session of the process — live ones plus the
// final values of those already closed. Populated by dave.Registry.Snapshot
// and handed straight to Observe.
//
// The counters are cumulative for the lifetime of the process; the gauges are
// point-in-time.
type DaveObservation struct {
	CommitsProcessed uint64
	CommitsFailed    uint64
	WelcomesJoined   uint64
	WelcomesFailed   uint64
	// RecoveriesMLS counts recoveries armed by a real MLS fault (rejected
	// commit or welcome); RecoveriesTransport counts those skipped because the
	// voice gateway was transiently down. Splitting them is the whole point:
	// the first means the crypto handshake broke, the second means the network
	// did, and only the first is a reason to suspect the DAVE implementation.
	RecoveriesMLS       uint64
	RecoveriesTransport uint64
	EncryptFailures     uint64
	DecryptFailures     uint64
	// PassthroughFrames counts frames forwarded unencrypted because no epoch
	// was active; TransitionFrames counts frames encrypted with the retained
	// previous ratchet during a re-key. TransitionFrames is the direct measure
	// of dave-go doing what libdave does not: carrying audio through the
	// window that goes silent on the default backend.
	PassthroughFrames uint64
	TransitionFrames  uint64
	TransitionWindows uint64
	ReplayRejected    uint64
	ProposalsRejected uint64
	Downgrades        uint64
	// DegradedSeconds accumulates time sessions spent without an active epoch
	// across recovery cycles that succeeded. TransportRetrySeconds accumulates
	// time spent in send-retry backoff because the voice gateway was down.
	DegradedSeconds       float64
	TransportRetrySeconds float64

	SessionsLive     int64
	SessionsDegraded int64
}

// DaveMetrics tracks DAVE end-to-end-encryption session health.
//
// Every instrument is observable: the underlying counters live inside the DAVE
// sessions and are read by polling them, not by incrementing at the call site.
// The cumulative ones are ObservableCounters rather than gauges so a restart
// (or a session ending) reads as a counter reset instead of a cliff, and rate()
// works on them.
//
// Only the dave-go backend feeds these. Under the default libdave backend no
// callback is registered and no series is produced — see dave.Registry.
type DaveMetrics struct {
	meter             metric.Meter // retained for RegisterObservers
	commits           metric.Int64ObservableCounter
	welcomes          metric.Int64ObservableCounter
	recoveries        metric.Int64ObservableCounter
	frameFailures     metric.Int64ObservableCounter
	frames            metric.Int64ObservableCounter
	transitionWindows metric.Int64ObservableCounter
	rejections        metric.Int64ObservableCounter
	downgrades        metric.Int64ObservableCounter
	degradedSeconds   metric.Float64ObservableCounter
	retrySeconds      metric.Float64ObservableCounter
	sessionsLive      metric.Int64ObservableGauge
	sessionsDegraded  metric.Int64ObservableGauge
}

func (d *DaveMetrics) init(meter metric.Meter) (err error) {
	d.meter = meter
	if d.commits, err = meter.Int64ObservableCounter("gdc.dave.commits.total",
		metric.WithDescription("MLS commits handled by DAVE sessions. Labels: result=processed|failed."),
	); err != nil {
		return
	}
	if d.welcomes, err = meter.Int64ObservableCounter("gdc.dave.welcomes.total",
		metric.WithDescription("MLS welcomes handled by DAVE sessions. Labels: result=joined|failed."),
	); err != nil {
		return
	}
	if d.recoveries, err = meter.Int64ObservableCounter("gdc.dave.recoveries.total",
		metric.WithDescription("DAVE session recoveries armed. Labels: cause=mls|transport."),
	); err != nil {
		return
	}
	if d.frameFailures, err = meter.Int64ObservableCounter("gdc.dave.frame.failures.total",
		metric.WithDescription("Frames that failed DAVE crypto. Labels: op=encrypt|decrypt."),
	); err != nil {
		return
	}
	if d.frames, err = meter.Int64ObservableCounter("gdc.dave.frames.total",
		metric.WithDescription("Frames handled outside the steady-state path. Labels: kind=passthrough|transition."),
	); err != nil {
		return
	}
	if d.transitionWindows, err = meter.Int64ObservableCounter("gdc.dave.transition.windows.total",
		metric.WithDescription("Post-activation windows in which the previous send ratchet was retained (one per epoch re-key)."),
	); err != nil {
		return
	}
	if d.rejections, err = meter.Int64ObservableCounter("gdc.dave.rejections.total",
		metric.WithDescription("Inputs refused by DAVE validation. Labels: kind=replay|proposal."),
	); err != nil {
		return
	}
	if d.downgrades, err = meter.Int64ObservableCounter("gdc.dave.downgrades.total",
		metric.WithDescription("Downgrades from E2EE to transport-only encryption (a non-supporting client joined)."),
	); err != nil {
		return
	}
	if d.degradedSeconds, err = meter.Float64ObservableCounter("gdc.dave.degraded.seconds.total",
		metric.WithDescription("Time DAVE sessions spent without an active epoch, across recoveries that succeeded."),
		metric.WithUnit("s"),
	); err != nil {
		return
	}
	if d.retrySeconds, err = meter.Float64ObservableCounter("gdc.dave.transport.retry.seconds.total",
		metric.WithDescription("Time DAVE sessions spent in send-retry backoff with the voice gateway down."),
		metric.WithUnit("s"),
	); err != nil {
		return
	}
	if d.sessionsLive, err = meter.Int64ObservableGauge("gdc.dave.sessions.live",
		metric.WithDescription("DAVE sessions currently attached to a voice connection."),
	); err != nil {
		return
	}
	if d.sessionsDegraded, err = meter.Int64ObservableGauge("gdc.dave.sessions.degraded",
		metric.WithDescription("Live DAVE sessions that expect E2EE but have no active epoch right now."),
	); err != nil {
		return
	}
	return nil
}

// RegisterObservers registers cb as the observable callback for every DAVE
// instrument.
func (d *DaveMetrics) RegisterObservers(cb metric.Callback) error {
	_, err := d.meter.RegisterCallback(cb,
		d.commits, d.welcomes, d.recoveries, d.frameFailures, d.frames,
		d.transitionWindows, d.rejections, d.downgrades,
		d.degradedSeconds, d.retrySeconds,
		d.sessionsLive, d.sessionsDegraded,
	)
	return err
}

// Observe emits one reading of every DAVE instrument via o.
// Call inside the callback registered with RegisterObservers.
func (d *DaveMetrics) Observe(o metric.Observer, s DaveObservation) {
	observeLabelled(o, d.commits, "result",
		labelled{"processed", s.CommitsProcessed}, labelled{"failed", s.CommitsFailed})
	observeLabelled(o, d.welcomes, "result",
		labelled{"joined", s.WelcomesJoined}, labelled{"failed", s.WelcomesFailed})
	observeLabelled(o, d.recoveries, "cause",
		labelled{"mls", s.RecoveriesMLS}, labelled{"transport", s.RecoveriesTransport})
	observeLabelled(o, d.frameFailures, "op",
		labelled{"encrypt", s.EncryptFailures}, labelled{"decrypt", s.DecryptFailures})
	observeLabelled(o, d.frames, "kind",
		labelled{"passthrough", s.PassthroughFrames}, labelled{"transition", s.TransitionFrames})
	observeLabelled(o, d.rejections, "kind",
		labelled{"replay", s.ReplayRejected}, labelled{"proposal", s.ProposalsRejected})

	o.ObserveInt64(d.transitionWindows, int64(s.TransitionWindows))
	o.ObserveInt64(d.downgrades, int64(s.Downgrades))
	o.ObserveFloat64(d.degradedSeconds, s.DegradedSeconds)
	o.ObserveFloat64(d.retrySeconds, s.TransportRetrySeconds)
	o.ObserveInt64(d.sessionsLive, s.SessionsLive)
	o.ObserveInt64(d.sessionsDegraded, s.SessionsDegraded)
}

// labelled is one attribute value and the counter total carried under it.
type labelled struct {
	value string
	total uint64
}

// observeLabelled emits each variant of one counter under the same attribute
// key. Zero totals are emitted too: an absent series and a series sitting at
// zero mean different things on a dashboard, and only the latter proves the
// path is being observed at all.
func observeLabelled(o metric.Observer, c metric.Int64ObservableCounter, key string, vals ...labelled) {
	for _, v := range vals {
		o.ObserveInt64(c, int64(v.total), metric.WithAttributes(attribute.String(key, v.value)))
	}
}
