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

	"github.com/sealbro/go-discord-caller/internal/telemetry"
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
	// PyroscopeEndpoint is the Pyroscope push URL (e.g. "http://alloy:4040"); empty disables profiling
	PyroscopeEndpoint string
	// LogLevel is the minimum log level (default: info); controlled by LOG_LEVEL env var
	LogLevel slog.Level
	// VoiceLogLevel is the minimum level for disgo's voice-layer logs, controlled
	// by VOICE_LOG_LEVEL. Defaults to off — see telemetry.VoiceLogger for why.
	VoiceLogLevel slog.Level
	// SessionIdleTimeout is how long a raid may go without a caller speaking to
	// someone in a speaker's channel before the session auto-stops.
	// Default: 10m. Set SESSION_IDLE_TIMEOUT=0 to disable.
	SessionIdleTimeout time.Duration
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
		slog.Warn("no speaker tokens configured; voice relay will not work — set DISCORD_SPEAKER_BOT_TOKEN_1 (and _2, _3 …)")
	}

	return &Config{
		OwnerBotToken:      ownerToken,
		SpeakerTokens:      speakerTokens,
		StorePath:          storePath(),
		OtelEndpoint:       os.Getenv("OTEL_ENDPOINT"),
		PyroscopeEndpoint:  os.Getenv("PYROSCOPE_ENDPOINT"),
		LogLevel:           parseLogLevel(os.Getenv("LOG_LEVEL")),
		VoiceLogLevel:      parseVoiceLogLevel(os.Getenv("VOICE_LOG_LEVEL")),
		SessionIdleTimeout: parseSessionIdleTimeout(os.Getenv("SESSION_IDLE_TIMEOUT")),
		Test: TestConfig{
			AllowBots: os.Getenv("TEST_ALLOW_BOTS") == "true",
		},
	}, nil
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

// loadSpeakerTokens scans all environment variables for DISCORD_SPEAKER_BOT_TOKEN_N
// keys (any numeric N), sorts them by index, and returns the tokens in order.
// Gaps in numbering (e.g. _1 and _3 with no _2) are silently skipped. Keys whose
// index overflows an int are skipped with a warning, and a token set under
// several keys is kept once, at its lowest index.
func loadSpeakerTokens() []string {
	type indexedToken struct {
		index int
		key   string
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
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			slog.Warn("ignoring speaker token with an unusable index", slog.String("key", key))
			continue
		}
		indexed = append(indexed, indexedToken{idx, key, val})
	}
	sort.Slice(indexed, func(i, j int) bool {
		if indexed[i].index != indexed[j].index {
			return indexed[i].index < indexed[j].index
		}
		return indexed[i].key < indexed[j].key
	})
	tokens := make([]string, 0, len(indexed))
	seen := make(map[string]string, len(indexed))
	for _, t := range indexed {
		if first, dup := seen[t.token]; dup {
			slog.Warn("ignoring duplicate speaker token",
				slog.String("key", t.key), slog.String("same_as", first))
			continue
		}
		seen[t.token] = t.key
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

// parseVoiceLogLevel parses VOICE_LOG_LEVEL (debug, info, warn, error, or off)
// into a level for disgo's voice logger. Empty, "off" and invalid input all
// disable it; the per-packet bursts it emits cost far more than they are worth
// unless someone is actively debugging audio.
func parseVoiceLogLevel(s string) slog.Level {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || strings.EqualFold(trimmed, "off") || strings.EqualFold(trimmed, "none") {
		return telemetry.LevelVoiceOff
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(trimmed))); err != nil {
		slog.Warn("invalid VOICE_LOG_LEVEL; voice logs stay disabled",
			slog.String("value", s),
		)
		return telemetry.LevelVoiceOff
	}
	return l
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
