package telemetry

import (
	"context"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/buildinfo"
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

const shutdownTimeout = 5 * time.Second

// LevelVoiceOff disables the voice logger entirely. It sits above every slog
// level, so no record from disgo's voice layer is ever enabled.
const LevelVoiceOff = slog.Level(math.MaxInt32)

// voiceLogLevel gates every logger VoiceLogger hands out. It is a LevelVar so
// the process-wide setting can be applied at startup while voice loggers are
// built lazily per bot, and read concurrently from voice goroutines.
var voiceLogLevel = func() *slog.LevelVar {
	v := new(slog.LevelVar)
	v.Set(LevelVoiceOff)
	return v
}()

// SetVoiceLogLevel applies the VOICE_LOG_LEVEL setting. Call it once, before
// any client is built. The choice is process-wide: disgo emits the same lines
// from the owner bot and every speaker bot, so a per-bot level would only make
// a voice outage harder to read.
func SetVoiceLogLevel(l slog.Level) { voiceLogLevel.Set(l) }

// VoiceLogger returns the logger handed to disgo's voice manager, tagged with
// the bot whose voice connections it covers.
//
// Disabled by default (LevelVoiceOff), because disgo logs voice failures
// per packet, not per connection: one connection that cannot decrypt emits
// "failed to DAVE decrypt packet" 50 times a second for as long as the raid
// lasts, which is tens of thousands of lines an hour on the OTLP pipeline for
// a single fact. The same applies to "failed to encrypt packet: missing key
// ratchet" on the send side. Metrics carry the signal those bursts stand in
// for; the log lines only carry the volume.
//
// Set VOICE_LOG_LEVEL=warn to turn them back on while debugging audio. That
// also restores the once-per-event diagnostics — gateway closes, reconnect
// attempts, UDP open failures — which a re-identify leaves no other trace of
// (see Service.watchVoiceReady). Below warn, disgo logs a line for every
// VoiceStateUpdate, so debug and info are for local runs only.
//
// botUserID is mandatory in practice: every line disgo emits from the voice
// layer is otherwise identical across the owner bot and all speaker bots, so a
// burst of "failed to encrypt packet: missing key ratchet" (one connection gone
// silent) cannot be attributed to a bot at all. Pass 0 only when the ID is
// genuinely unknown — the attribute is then omitted rather than logged as "0".
func VoiceLogger(botUserID snowflake.ID) *slog.Logger {
	logger := slog.New(levelHandler{
		level:   voiceLogLevel,
		Handler: slog.Default().Handler(),
	}).With(slog.String("component", "voice"))

	if botUserID == 0 {
		return logger
	}
	return logger.With(slog.String("botUserID", botUserID.String()))
}

// Setup initialises OpenTelemetry providers for traces, metrics and logs.
// All three signals are exported via OTLP gRPC to a single endpoint.
// Returns a shutdown function that flushes and closes all providers, each with
// its own timeout. When endpoint is empty, no-op providers are used and the
// returned shutdown does nothing. If setup fails midway, whatever was already
// created is shut down before the error is returned.
func Setup(ctx context.Context, endpoint string, level slog.Level) (shutdown func(), err error) {
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
	// Providers go first and the connection last, so they can still flush.
	var steps []shutdownFunc
	defer func() {
		if err != nil {
			shutdownAll(shutdownTimeout, append(steps, closeConn(conn))...)
		}
	}()

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(ServiceName), semconv.ServiceVersion(buildinfo.Version)),
	)
	if err != nil {
		return nil, err
	}

	// Traces
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, err
	}
	tp := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
	)
	steps = append(steps, tp.Shutdown)
	otel.SetTracerProvider(tp)

	// Metrics
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, err
	}
	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(15*time.Second))),
		metric.WithResource(res),
	)
	steps = append(steps, mp.Shutdown)
	otel.SetMeterProvider(mp)
	if err := startProcessMetrics(mp); err != nil {
		return nil, err
	}

	// Logs
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, err
	}
	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
		log.WithResource(res),
	)
	steps = append(steps, lp.Shutdown)
	olog.SetLoggerProvider(lp)

	// Replace default slog with OTel bridge so all slog calls export via OTLP
	// and automatically inject trace_id/span_id when context is provided.
	otelHandler := otelslog.NewHandler(ServiceName,
		otelslog.WithLoggerProvider(lp),
		otelslog.WithSource(true),
	)
	slog.SetDefault(slog.New(levelHandler{level: level, Handler: otelHandler}))

	return func() { shutdownAll(shutdownTimeout, append(steps, closeConn(conn))...) }, nil
}

type shutdownFunc func(context.Context) error

// shutdownAll runs each step with its own timeout so one stalled collector
// connection cannot leave the later providers unflushed.
func shutdownAll(budget time.Duration, fns ...shutdownFunc) {
	for _, fn := range fns {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		_ = fn(ctx)
		cancel()
	}
}

func closeConn(conn *grpc.ClientConn) shutdownFunc {
	return func(context.Context) error { return conn.Close() }
}
