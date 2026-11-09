// Package spantest lets tests read the spans started through telemetry.Tracer.
package spantest

import (
	"sync"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	once     sync.Once
	recorder *tracetest.SpanRecorder
)

// Record returns a function that lists the spans ended since Record was called.
//
// The recorder is installed once per process: telemetry.Tracer is created at
// startup, and the global provider only hands a recorder to tracers that exist
// already on the first SetTracerProvider call, so a second call is ignored
// and a test relying on it fails on every repeat.
func Record() func() []sdktrace.ReadOnlySpan {
	once.Do(func() {
		recorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	})
	start := len(recorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return recorder.Ended()[start:] }
}
