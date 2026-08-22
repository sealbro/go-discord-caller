module github.com/sealbro/go-discord-caller

go 1.26

require (
	github.com/disgoorg/disgo v0.19.3
	github.com/disgoorg/godave/golibdave v0.3.0
	github.com/disgoorg/snowflake/v2 v2.0.3
	github.com/hraban/opus v0.0.0-20260708213942-bde8e4304501
	github.com/joho/godotenv v1.5.1
	github.com/nicksnyder/go-i18n/v2 v2.6.1
	go.opentelemetry.io/contrib/bridges/otelslog v0.20.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.21.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.45.0
	go.opentelemetry.io/otel/log v0.21.0
	go.opentelemetry.io/otel/metric v1.45.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.opentelemetry.io/otel/sdk/log v0.21.0
	go.opentelemetry.io/otel/sdk/metric v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
	golang.org/x/text v0.41.0
	google.golang.org/grpc v1.83.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/disgoorg/godave v0.3.0 // indirect
	github.com/disgoorg/godave/libdave v0.3.0 // indirect
	github.com/disgoorg/json/v2 v2.0.0 // indirect
	github.com/disgoorg/omit v1.0.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/sasha-s/go-csync v0.0.0-20240107134140-fcbab37b09ad // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.45.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

// Pinned to v0.19.3: v0.19.4-v0.19.6 break bot reconnection when moved between
// voice channels. Drop these excludes once a release later than v0.19.6 lands.
exclude (
	github.com/disgoorg/disgo v0.19.4
	github.com/disgoorg/disgo v0.19.5
	github.com/disgoorg/disgo v0.19.6
)
