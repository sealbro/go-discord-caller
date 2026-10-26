package telemetry

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A rejected upload used to be dropped silently, so a wrong Grafana Cloud
// token left profiling dead with nothing in the logs to say so.
func TestProfilingUploadFailureIsLoggedAsWarning(t *testing.T) {
	buf := captureDefaultLogger(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	stop, err := StartProfiling(srv.URL)
	if err != nil {
		t.Fatalf("StartProfiling: %v", err)
	}
	stop()

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "401") {
		t.Fatalf("want a WARN line carrying the 401, got:\n%s", out)
	}
}

// Uploads run every 15 s per profile type, so a lasting failure must not turn
// into a steady stream of identical lines.
func TestProfilingUploadFailuresAreThrottled(t *testing.T) {
	buf := captureDefaultLogger(t)
	now := time.Unix(0, 0)
	logger := newProfilingLogger(slog.Default(), time.Minute)
	logger.now = func() time.Time { return now }

	for range 5 {
		logger.Errorf("upload profile: %v", "failed to upload: (401) 'Unauthorized'")
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("want 1 warning within the interval, got %d:\n%s", got, buf.String())
	}

	now = now.Add(time.Minute)
	logger.Errorf("upload profile: %v", "failed to upload: (401) 'Unauthorized'")
	out := buf.String()
	if got := strings.Count(out, "level=WARN"); got != 2 {
		t.Fatalf("want a second warning after the interval, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "suppressed=4") {
		t.Fatalf("want the second warning to count the 4 suppressed failures, got:\n%s", out)
	}
}
