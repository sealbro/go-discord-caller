package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

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

// Load reads configuration from a .env file (if present) and then from environment variables.
// Environment variables already set in the process take precedence over the .env file.
//
// Owner bot:
//
//	DISCORD_OWNER_BOT_TOKEN  (required)
//
// Speaker pool (at least one recommended):
//
//	DISCORD_SPEAKER_1_BOT_TOKEN
//	DISCORD_SPEAKER_2_BOT_TOKEN
//	... (sequential, stops at first missing index)
func Load() (*Config, error) {
	// Load .env file if it exists; silently skip if not found.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("error reading .env file: %w", err)
	}

	ownerToken := os.Getenv("DISCORD_OWNER_BOT_TOKEN")
	if ownerToken == "" {
		return nil, fmt.Errorf("DISCORD_OWNER_BOT_TOKEN environment variable is required")
	}

	var speakerTokens []string
	for i := 1; ; i++ {
		key := "DISCORD_SPEAKER_" + strconv.Itoa(i) + "_BOT_TOKEN"
		token := os.Getenv(key)
		if token == "" {
			break
		}
		speakerTokens = append(speakerTokens, token)
	}

	if len(speakerTokens) == 0 {
		slog.Warn("no speaker tokens configured; voice relay will not work — set DISCORD_SPEAKER_1_BOT_TOKEN (and _2, _3 …)")
	}

	return &Config{
		OwnerBotToken: ownerToken,
		SpeakerTokens: speakerTokens,
		StorePath:     storePath(),
	}, nil
}

// storePath returns the YAML store file path from STORE_PATH, defaulting to "store.yaml".
func storePath() string {
	if p := os.Getenv("STORE_PATH"); p != "" {
		return p
	}
	return "store.yaml"
}
