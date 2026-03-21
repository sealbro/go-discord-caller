package config

import (
	"fmt"
	"os"
)

// Config holds all application configuration.
type Config struct {
	// Discord bot token (required)
	BotToken string
	// Guild ID to restrict commands to a specific server (optional, empty = global)
	GuildID string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("DISCORD_BOT_TOKEN environment variable is required")
	}

	return &Config{
		BotToken: token,
		GuildID:  os.Getenv("DISCORD_GUILD_ID"),
	}, nil
}
