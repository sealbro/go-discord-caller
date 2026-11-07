//go:build !race

// disgo v0.20.0 reads Gateway.heartbeatCancel in CloseWithCode while the
// heartbeat goroutine writes it, so every reconnect trips the race detector
// regardless of our code. Drop this constraint once that is fixed upstream.

package pool

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/dave"
	"github.com/sealbro/go-discord-caller/internal/gatewaytest"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// A speaker bot whose gateway keeps invalidating its session must not be
// re-identified once a second: each speaker has its own 1,000 IDENTIFYs a day.
func TestSpeakerGatewaySlowsDownWhenSessionKeepsBeingInvalidated(t *testing.T) {
	wsURL, identifies := gatewaytest.IdentifyingGateway(t)

	client, err := newPoolClient("MQ.fake.token", &telemetry.VoiceRegionMetrics{}, dave.NewStats(), dave.NewEncryptStalls(),
		func(snowflake.ID) {},
		bot.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		bot.WithGatewayConfigOpts(gateway.WithURL(wsURL), gateway.WithCompression(gateway.CompressionNone)),
	)
	if err != nil {
		t.Fatalf("newPoolClient: %v", err)
	}
	openCtx, cancelOpen := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOpen()
	if err := client.OpenGateway(openCtx); err != nil {
		t.Fatalf("OpenGateway: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.Close(closeCtx)
	})

	gatewaytest.RequireGrowingGaps(t, identifies)
}
