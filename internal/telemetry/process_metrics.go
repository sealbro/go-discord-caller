package telemetry

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/metric"
)

// startProcessMetrics exports the Go runtime metrics (go.memory.*,
// go.goroutine.count, …) plus the process's CPU time and resident memory.
// RSS is the figure to watch against the container limit: libdave and libopus
// allocate outside the Go heap, so go.memory.used alone undercounts.
func startProcessMetrics(mp metric.MeterProvider) error {
	if err := otelruntime.Start(
		otelruntime.WithMeterProvider(mp),
		otelruntime.WithMinimumReadMemStatsInterval(15*time.Second),
	); err != nil {
		return err
	}

	meter := mp.Meter(ServiceName)
	cpuTime, err := meter.Float64ObservableCounter("gdc.process.cpu.time",
		metric.WithDescription("CPU time consumed by the process, user plus system."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}
	rss, err := meter.Int64ObservableGauge("gdc.process.memory.usage",
		metric.WithDescription("Resident set size of the process, including memory allocated by C libraries. Linux only."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
			o.ObserveFloat64(cpuTime, time.Duration(ru.Utime.Nano()+ru.Stime.Nano()).Seconds())
		}
		if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
			if n, err := residentBytes(statm, os.Getpagesize()); err == nil {
				o.ObserveInt64(rss, n)
			}
		}
		return nil
	}, cpuTime, rss)
	return err
}

// residentBytes parses /proc/self/statm, whose second field is the resident
// set size in pages.
func residentBytes(statm []byte, pageSize int) (int64, error) {
	fields := bytes.Fields(statm)
	if len(fields) < 2 {
		return 0, errors.New("statm: missing resident field")
	}
	pages, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(pageSize), nil
}
