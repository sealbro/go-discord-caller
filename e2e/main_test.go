//go:build e2e

package e2e

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

// h is shared across all tests; created once in TestMain.
var h *Harness

func TestMain(m *testing.M) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("e2e config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h, err = newHarness(ctx, cfg)
	if err != nil {
		log.Fatalf("e2e harness: %v", err)
	}

	code := m.Run()

	h.Shutdown(context.Background())
	os.Exit(code)
}
