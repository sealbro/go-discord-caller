//go:build !race

// disgo v0.20.0 reads Gateway.heartbeatCancel in CloseWithCode while the
// heartbeat goroutine writes it, so every reconnect trips the race detector
// regardless of our code. Drop this constraint once that is fixed upstream.

package bot

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"

	"github.com/sealbro/go-discord-caller/internal/dave"
	"github.com/sealbro/go-discord-caller/internal/gatewaytest"
)

// Production, 2026-11-07: a gateway that kept dropping the owner after READY was
// re-identified every few seconds for 90 minutes. That used up Discord's 1,000
// IDENTIFYs a day, which resets the bot token and left the bot offline. disgo
// restarts its reconnect backoff at one second after every READY, so a gateway
// that is ready but unstable is never slowed down.
func TestGatewaySlowsDownWhenSessionKeepsBeingInvalidated(t *testing.T) {
	wsURL, identifies := gatewaytest.IdentifyingGateway(t)

	client, err := NewOwnerClient("MQ.fake.token", dave.NewStats(), dave.NewEncryptStalls(),
		bot.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		bot.WithGatewayConfigOpts(gateway.WithURL(wsURL), gateway.WithCompression(gateway.CompressionNone)),
	)
	if err != nil {
		t.Fatalf("NewOwnerClient: %v", err)
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
