package telemetry

import (
	"github.com/grafana/pyroscope-go"
	"github.com/sealbro/go-discord-caller/internal/buildinfo"
)

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
