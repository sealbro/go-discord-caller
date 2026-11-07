package bot

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/store"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOwnerIsKnownToMetricsBeforeItsGatewayOpens(t *testing.T) {
	const ownerID snowflake.ID = 8100
	token := base64.RawURLEncoding.EncodeToString([]byte(ownerID.String())) + ".AAAAAA.not-a-real-secret"
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	b, err := New(&config.Config{OwnerBotToken: token}, store.NewInMemoryStore(), meter)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.poolSvc.StartMetrics()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	known := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || m.Name != "gdc.discord.bot" {
				continue
			}
			for _, dp := range gauge.DataPoints {
				id, _ := dp.Attributes.Value("bot_id")
				known[id.AsString()] = true
			}
		}
	}

	if !known[ownerID.String()] {
		t.Errorf("gdc.discord.bot lists %v, want the owner %s before its gateway opens", known, ownerID)
	}
}
