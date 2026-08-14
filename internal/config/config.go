package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/sealbro/go-discord-caller/internal/dave"
)

// TestConfig holds optional test overrides.
type TestConfig struct {
	// AllowBots disables the bot-user filter in allowUser so that bot accounts
	// with the caller role can be captured. Set TEST_ALLOW_BOTS=true for E2E tests.
	AllowBots bool
}

// Config holds all application configuration.
type Config struct {
	// OwnerBotToken is the manager bot token (required)
	OwnerBotToken string
	// SpeakerTokens is the ordered pool of speaker bot tokens loaded from env
	SpeakerTokens []string
	// StorePath is the path to the YAML persistence file (default: store.yaml)
	StorePath string
	// OtelEndpoint is the OTLP gRPC endpoint (e.g. "alloy:4317"); empty disables telemetry
	OtelEndpoint string
	// LogLevel is the minimum log level (default: info); controlled by LOG_LEVEL env var
	LogLevel slog.Level
	// SessionIdleTimeout is how long every voice channel in a raid may stay empty
	// of non-bot users (nobody connected) before the session auto-stops.
	// Default: 10m. Set SESSION_IDLE_TIMEOUT=0 to disable.
	SessionIdleTimeout time.Duration
	// DaveImpl selects the DAVE E2EE implementation for all voice connections
	// (DAVE_IMPL: "libdave" — default — or "dave-go").
	DaveImpl dave.Impl
	// Test holds optional test/debug audio overrides
	Test TestConfig
}

const defaultSessionIdleTimeout = 10 * time.Minute

var speakerTokenPattern = regexp.MustCompile(`^DISCORD_SPEAKER_BOT_TOKEN_(\d+)$`)

// Load reads configuration from a .env file (if present) and then from environment variables.
// Environment variables already set in the process take precedence over the .env file.
//
// Owner bot:
//
//	DISCORD_OWNER_BOT_TOKEN  (required)
//
// Speaker pool (at least one recommended):
//
//	DISCORD_SPEAKER_BOT_TOKEN_1
//	DISCORD_SPEAKER_BOT_TOKEN_2
//	... (any numeric suffix; gaps in numbering are supported)
func Load() (*Config, error) {
	// Load .env file if it exists; silently skip if not found.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("error reading .env file: %w", err)
	}

	ownerToken := os.Getenv("DISCORD_OWNER_BOT_TOKEN")
	if ownerToken == "" {
		return nil, fmt.Errorf("DISCORD_OWNER_BOT_TOKEN environment variable is required")
	}

	speakerTokens := loadSpeakerTokens()
	if len(speakerTokens) == 0 {
		slog.Warn("no speaker tokens configured; voice relay will not work — set DISCORD_SPEAKER_1_BOT_TOKEN (and _2, _3 …)")
	}

	return &Config{
		OwnerBotToken:      ownerToken,
		SpeakerTokens:      speakerTokens,
		StorePath:          storePath(),
		OtelEndpoint:       os.Getenv("OTEL_ENDPOINT"),
		LogLevel:           parseLogLevel(os.Getenv("LOG_LEVEL")),
		SessionIdleTimeout: parseSessionIdleTimeout(os.Getenv("SESSION_IDLE_TIMEOUT")),
		DaveImpl:           parseDaveImpl(os.Getenv("DAVE_IMPL")),
		Test: TestConfig{
			AllowBots: os.Getenv("TEST_ALLOW_BOTS") == "true",
		},
	}, nil
}

// parseDaveImpl parses DAVE_IMPL into a dave.Impl. Empty selects the default
// (libdave). An unknown value falls back to the default with a warning rather
// than refusing to start: the process is a voice relay, and a typo in an
// optional tuning knob should not take it offline.
func parseDaveImpl(s string) dave.Impl {
	impl, err := dave.Parse(s)
	if err != nil {
		slog.Warn("invalid DAVE_IMPL; using default",
			slog.String("value", s),
			slog.String("default", string(dave.Default)),
			slog.Any("err", err),
		)
	}
	return impl
}

// parseSessionIdleTimeout parses SESSION_IDLE_TIMEOUT (e.g. "10m", "0s") into a
// Duration. Empty defaults to defaultSessionIdleTimeout; "0" disables.
// Invalid input falls back to the default with a warning.
func parseSessionIdleTimeout(s string) time.Duration {
	if s == "" {
		return defaultSessionIdleTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		slog.Warn("invalid SESSION_IDLE_TIMEOUT; using default",
			slog.String("value", s),
			slog.Duration("default", defaultSessionIdleTimeout),
			slog.Any("err", err),
		)
		return defaultSessionIdleTimeout
	}
	return d
}

// loadSpeakerTokens scans all environment variables for DISCORD_SPEAKER_N_BOT_TOKEN
// keys (any numeric N), sorts them by index, and returns the tokens in order.
// Gaps in numbering (e.g. _1 and _3 with no _2) are silently skipped.
func loadSpeakerTokens() []string {
	type indexedToken struct {
		index int
		token string
	}
	var indexed []indexedToken
	for _, env := range os.Environ() {
		eqIdx := strings.IndexByte(env, '=')
		if eqIdx < 0 {
			continue
		}
		key, val := env[:eqIdx], env[eqIdx+1:]
		if val == "" {
			continue
		}
		m := speakerTokenPattern.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		indexed = append(indexed, indexedToken{idx, val})
	}
	sort.Slice(indexed, func(i, j int) bool {
		return indexed[i].index < indexed[j].index
	})
	tokens := make([]string, 0, len(indexed))
	for _, t := range indexed {
		tokens = append(tokens, t.token)
	}
	return tokens
}

// storePath returns the YAML store file path from STORE_PATH, defaulting to "store.yaml".
func storePath() string {
	if p := os.Getenv("STORE_PATH"); p != "" {
		return p
	}
	return "store.yaml"
}

// parseLogLevel parses a log level string (debug, info, warn, error) into slog.Level.
// Defaults to LevelInfo on empty or invalid input.
func parseLogLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}
