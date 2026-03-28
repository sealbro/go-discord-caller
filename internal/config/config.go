package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all application configuration.
type Config struct {
	// OwnerBotToken is the manager bot token (required)
	OwnerBotToken string
	// SpeakerTokens is the ordered pool of speaker bot tokens loaded from env
	SpeakerTokens []string
	// StorePath is the path to the YAML persistence file (default: store.yaml)
	StorePath string
}

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
		OwnerBotToken: ownerToken,
		SpeakerTokens: speakerTokens,
		StorePath:     storePath(),
	}, nil
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
