package dave

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func decryptDurationPoints(t *testing.T, reader *metric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.dave.decrypt.duration" {
				continue
			}
			h, ok := md.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("gdc.dave.decrypt.duration: unexpected data type %T", md.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// Frames from every speaker are decrypted one after another on disgo's single
// receive goroutine, so a decrypt that blocks delays all of them. Timing it
// tells that apart from frames that were already late when they arrived.
func TestDecryptDurationIsExported(t *testing.T) {
	stats := NewStats()
	reader, _ := newReader(t, stats)
	session, backend := knownSession(t, stats, "bot-1", nil, "user-a")
	backend.delay = 30 * time.Millisecond

	if _, err := session.Decrypt("user-a", nil, make([]byte, 64)); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	points := decryptDurationPoints(t, reader)
	if len(points) != 1 {
		t.Fatalf("got %d gdc.dave.decrypt.duration series, want 1", len(points))
	}
	p := points[0]
	if p.Count != 1 {
		t.Errorf("count = %d, want 1", p.Count)
	}
	if p.Sum < 30 {
		t.Errorf("sum = %.1f ms, want at least the backend's 30 ms", p.Sum)
	}
	if bot, _ := p.Attributes.Value(attribute.Key("bot_user_id")); bot.AsString() != "bot-1" {
		t.Errorf("bot_user_id = %q, want bot-1", bot.AsString())
	}
}

func TestDecryptBeforeMetricsAreAttached(t *testing.T) {
	stats := NewStats()
	session, _ := knownSession(t, stats, "bot-1", nil, "user-a")

	if _, err := session.Decrypt("user-a", nil, make([]byte, 64)); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
}
