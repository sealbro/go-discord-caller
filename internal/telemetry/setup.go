package telemetry

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/disgoorg/snowflake/v2"
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

// levelHandler wraps a slog.Handler and gates Enabled() by a minimum level.
type levelHandler struct {
	level slog.Leveler
	slog.Handler
}

func (h levelHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level.Level() && h.Handler.Enabled(ctx, l)
}

func (h levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelHandler{level: h.level, Handler: h.Handler.WithAttrs(attrs)}
}

func (h levelHandler) WithGroup(name string) slog.Handler {
	return levelHandler{level: h.level, Handler: h.Handler.WithGroup(name)}
}

const ServiceName = "go-discord-caller"

// VoiceLogger returns the logger handed to disgo's voice manager, tagged with
// the bot whose voice connections it covers.
//
// This used to be slog.DiscardHandler, which silently dropped every diagnostic
// from the voice layer — gateway closes, reconnect attempts, UDP open failures,
// encryption-mode errors. Voice connections break far more often than the
// application layer can observe (a re-identify leaves no application-visible
// trace at all; see Service.watchVoiceReady), so those lines are the only
// direct evidence when audio goes silent.
//
// Gated at Warn: disgo's voice Debug output is per-event and includes a line
// for every VoiceStateUpdate, which would swamp the OTLP log pipeline. Warn and
// above is exactly the set worth exporting.
//
// botUserID is mandatory in practice: every line disgo emits from the voice
// layer is otherwise identical across the owner bot and all speaker bots, so a
// burst of "failed to encrypt packet: missing key ratchet" (one connection gone
// silent) cannot be attributed to a bot at all. Pass 0 only when the ID is
// genuinely unknown — the attribute is then omitted rather than logged as "0".
func VoiceLogger(botUserID snowflake.ID) *slog.Logger {
	logger := slog.New(levelHandler{
		level:   slog.LevelWarn,
		Handler: slog.Default().Handler(),
	}).With(slog.String("component", "voice"))

	if botUserID == 0 {
		return logger
	}
	return logger.With(slog.String("botUserID", botUserID.String()))
}

// Setup initialises OpenTelemetry providers for traces, metrics and logs.
// All three signals are exported via OTLP gRPC to a single endpoint.
// Returns a shutdown function that flushes and closes all providers.
// When endpoint is empty, no-op providers are used and nil shutdown is returned.
func Setup(ctx context.Context, endpoint string, level slog.Level) (func(), error) {
	if endpoint == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     level,
		})))
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
		resource.WithAttributes(semconv.ServiceName(ServiceName)),
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
	otelHandler := otelslog.NewHandler(ServiceName,
		otelslog.WithLoggerProvider(lp),
		otelslog.WithSource(true),
	)
	slog.SetDefault(slog.New(levelHandler{level: level, Handler: otelHandler}))

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
