package telemetry

import "go.opentelemetry.io/otel"

// Tracer is the package-level tracer used for creating spans.
var Tracer = otel.Tracer(ServiceName)
