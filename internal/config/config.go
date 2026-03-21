package config

import (
	"fmt"
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
	// GuildID restricts slash command registration to one server (optional, empty = global)
	GuildID string
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

	return &Config{
		OwnerBotToken: ownerToken,
		SpeakerTokens: speakerTokens,
		GuildID:       os.Getenv("DISCORD_GUILD_ID"),
	}, nil
}
