package manager

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans swaps in a recording tracer provider for the duration of a test.
// telemetry.Tracer comes from the otel global, which delegates to whichever
// provider is installed, so registering one here is enough.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec
}

func TestStartPhaseNestsAndRecordsError(t *testing.T) {
	rec := recordSpans(t)

	outerCtx, endOuter := startPhase(context.Background(), "voice.session.setup")
	_, endInner := startPhase(outerCtx, "voice.conn.open")
	endInner(errors.New("join timeout"))
	endOuter(nil)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("want 2 ended spans, got %d", len(spans))
	}
	inner, outer := spans[0], spans[1]
	if inner.Name() != "voice.conn.open" || outer.Name() != "voice.session.setup" {
		t.Fatalf("unexpected span names: %q, %q", inner.Name(), outer.Name())
	}
	if inner.Parent().SpanID() != outer.SpanContext().SpanID() {
		t.Error("inner phase must be a child of the outer phase")
	}
	if inner.Status().Code != codes.Error {
		t.Errorf("failed phase must carry an error status, got %v", inner.Status().Code)
	}
	if len(inner.Events()) == 0 {
		t.Error("failed phase must record the error as an event")
	}
	if outer.Status().Code == codes.Error {
		t.Error("a successful phase must not be marked as an error")
	}
}
