package telemetry

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/grafana/pyroscope-go"
	"github.com/sealbro/go-discord-caller/internal/buildinfo"
)

const profilingWarnInterval = 10 * time.Minute

// StartProfiling pushes continuous profiles to a Pyroscope-compatible endpoint
// (Pyroscope itself, or Alloy's pyroscope.receive_http). Returns a stop func
// that flushes the last upload. When endpoint is empty, profiling is disabled.
//
// Block and mutex profiles are left out: they need runtime sampling rates that
// add cost to every contended lock on the audio path.
func StartProfiling(endpoint string) (func(), error) {
	if endpoint == "" {
		return func() {}, nil
	}

	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: ServiceName,
		ServerAddress:   endpoint,
		Logger:          newProfilingLogger(slog.Default(), profilingWarnInterval),
		Tags:            map[string]string{"version": buildinfo.Version},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
		},
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = profiler.Stop() }, nil
}

// profilingLogger reports the profiler's errors — in practice, rejected
// uploads — as throttled warnings. A lasting failure such as a bad token
// would otherwise repeat on every upload, several times every 15 s.
type profilingLogger struct {
	log   *slog.Logger
	every time.Duration
	now   func() time.Time

	mu         sync.Mutex
	lastWarn   time.Time
	suppressed int
}

func newProfilingLogger(log *slog.Logger, every time.Duration) *profilingLogger {
	return &profilingLogger{log: log, every: every, now: time.Now}
}

func (l *profilingLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	now := l.now()
	if !l.lastWarn.IsZero() && now.Sub(l.lastWarn) < l.every {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.lastWarn, l.suppressed = now, 0
	l.mu.Unlock()

	l.log.Warn("profiling: pyroscope error",
		slog.String("err", fmt.Sprintf(format, args...)),
		slog.Int("suppressed", suppressed),
	)
}

func (*profilingLogger) Infof(string, ...any) {}

func (*profilingLogger) Debugf(string, ...any) {}
