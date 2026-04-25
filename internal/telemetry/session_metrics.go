package telemetry

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// SessionMetrics tracks voice raid session lifecycle and fanout health.
type SessionMetrics struct {
	active   metric.Int64UpDownCounter
	starts   metric.Int64Counter
	stops    metric.Int64Counter
	speakers metric.Int64Gauge
	dropped  metric.Int64Counter
}

func (s *SessionMetrics) init(meter metric.Meter) (err error) {
	if s.active, err = meter.Int64UpDownCounter("gdc.voice.sessions.active",
		metric.WithDescription("Currently active voice raid sessions"),
	); err != nil {
		return
	}
	if s.starts, err = meter.Int64Counter("gdc.voice.session.start.total",
		metric.WithDescription("Voice raid starts"),
	); err != nil {
		return
	}
	if s.stops, err = meter.Int64Counter("gdc.voice.session.stop.total",
		metric.WithDescription("Voice raid stops"),
	); err != nil {
		return
	}
	if s.speakers, err = meter.Int64Gauge("gdc.session.speakers",
		metric.WithDescription("Number of speaker bots that joined the active voice raid session."),
	); err != nil {
		return
	}
	if s.dropped, err = meter.Int64Counter("gdc.fanout.frames.dropped.total",
		metric.WithDescription("Opus frames dropped due to full channels in the fanout/relay pipeline."),
	); err != nil {
		return
	}
	return nil
}

// SessionStarted records the start of a voice raid: increments active counter,
// start total, and sets the speaker gauge for guildID.
func (s *SessionMetrics) SessionStarted(ctx context.Context, guildID snowflake.ID, speakerCount int) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.active.Add(ctx, 1, attrs)
	s.starts.Add(ctx, 1, attrs)
	s.speakers.Record(ctx, int64(speakerCount), attrs)
}

// SessionStopped records the end of a voice raid: resets speaker gauge to 0,
// decrements active counter, and increments stop total.
func (s *SessionMetrics) SessionStopped(ctx context.Context, guildID snowflake.ID) {
	attrs := metric.WithAttributes(attribute.String("guild_id", guildID.String()))
	s.speakers.Record(ctx, 0, attrs)
	s.active.Add(ctx, -1, attrs)
	s.stops.Add(ctx, 1, attrs)
}

// DropOption pre-computes a MeasurementOption for FrameDropped.
// Call once outside hot loops to avoid per-frame attribute allocations.
func (s *SessionMetrics) DropOption(guildID snowflake.ID, path string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("guild_id", guildID.String()),
		attribute.String("path", path),
	)
}

// FrameDropped records one dropped Opus frame using a pre-computed option from DropOption.
func (s *SessionMetrics) FrameDropped(ctx context.Context, opt metric.MeasurementOption) {
	s.dropped.Add(ctx, 1, opt)
}
