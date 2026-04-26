package telemetry

import "go.opentelemetry.io/otel/metric"

// Metrics groups all application instruments by domain.
// Create with NewMetrics; inject via constructor instead of accessing globals.
type Metrics struct {
	Session SessionMetrics
	Mixer   MixerMetrics
	Pool    PoolMetrics
	Bot     BotMetrics
	Opus    OpusMetrics
}

// NewMetrics initialises all instruments from the provided meter.
// Returns an error instead of panicking so startup failures are handled gracefully.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	if err := m.Session.init(meter); err != nil {
		return nil, err
	}
	if err := m.Mixer.init(meter); err != nil {
		return nil, err
	}
	if err := m.Pool.init(meter); err != nil {
		return nil, err
	}
	if err := m.Bot.init(meter); err != nil {
		return nil, err
	}
	if err := m.Opus.init(meter); err != nil {
		return nil, err
	}
	return m, nil
}
