//go:build integration || stress

package integration

import (
	"context"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/integration/test"
)

// h is shared across all tests; created once in TestMain.
var h *test.Harness

func TestMain(m *testing.M) {
	// Silence application slog output (the bots log a line per voice event) so
	// test output stays readable. Set TEST_LOG=1 to keep logs when debugging.
	if os.Getenv("TEST_LOG") == "" {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}

	cfg, err := test.LoadConfig()
	if err != nil {
		log.Fatalf("integration config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h, err = test.NewHarness(ctx, cfg)
	if err != nil {
		log.Fatalf("integration harness: %v", err)
	}

	code := m.Run()

	h.Shutdown(context.Background())
	os.Exit(code)
}
