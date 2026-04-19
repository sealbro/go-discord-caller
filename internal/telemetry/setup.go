package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	olog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const serviceName = "go-discord-caller"

// Setup initialises OpenTelemetry providers for traces, metrics and logs.
// All three signals are exported via OTLP gRPC to a single endpoint.
// Returns a shutdown function that flushes and closes all providers.
// When endpoint is empty, no-op providers are used and nil shutdown is returned.
func Setup(ctx context.Context, endpoint string) (func(), error) {
	if endpoint == "" {
		return func() {}, nil
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	// Close the connection on any setup error so we don't leak it.
	var setupErr error
	defer func() {
		if setupErr != nil {
			_ = conn.Close()
		}
	}()

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		setupErr = err
		return nil, err
	}

	// Traces
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		setupErr = err
		return nil, err
	}
	tp := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Metrics
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		setupErr = err
		return nil, err
	}
	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(15*time.Second))),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// Logs
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		setupErr = err
		return nil, err
	}
	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
		log.WithResource(res),
	)
	olog.SetLoggerProvider(lp)

	// Replace default slog with OTel bridge so all slog calls export via OTLP
	// and automatically inject trace_id/span_id when context is provided.
	slog.SetDefault(otelslog.NewLogger(serviceName,
		otelslog.WithLoggerProvider(lp),
		otelslog.WithSource(true),
	))

	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutCtx)
		_ = mp.Shutdown(shutCtx)
		_ = lp.Shutdown(shutCtx)
		_ = conn.Close()
	}

	return shutdown, nil
}
