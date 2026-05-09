//go:build integration

package test

import (
	"fmt"
	"os"

	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
	"github.com/sealbro/go-discord-caller/internal/config"
)

// Config extends the production config with integration-test-specific fields.
type Config struct {
	config.Config                  // OwnerBotToken, SpeakerTokens (loaded via config.Load)
	SourceToken       string       // E2E_SOURCE_BOT_TOKEN — bot that plays audio as the caller
	SourceToken2      string       // E2E_SOURCE_BOT_TOKEN_2 — optional; enables E2/E6
	ListenerToken     string       // E2E_LISTENER_BOT_TOKEN — bot that counts received frames
	GuildID           snowflake.ID // E2E_TEST_GUILD_ID
	GuestGuildID      snowflake.ID // E2E_GUEST_GUILD_ID — optional; enables E5
	OwnerChannelID    snowflake.ID // E2E_OWNER_CHANNEL_ID
	Speaker1ChannelID snowflake.ID // E2E_SPEAKER_CHANNEL_ID
	Speaker2ChannelID snowflake.ID // E2E_SPEAKER2_CHANNEL_ID — optional; enables E2/E6
	CallerRoleID      snowflake.ID // E2E_CALLER_ROLE_ID
	// Guest guild channels (required only when GuestGuildID is set)
	GuestOwnerChannelID   snowflake.ID
	GuestSpeakerChannelID snowflake.ID
	// Directory containing .dca files streamed in random order by the source bot.
	SamplesDir string
}

// LoadConfig loads the integration-test configuration.
// Common fields (owner token, speaker pool) are loaded via config.Load so the
// speaker-token scan logic is not duplicated. Integration-specific fields are
// read from additional env vars prefixed with E2E_.
func LoadConfig() (*Config, error) {
	// go test sets cwd to the package directory (integration/); .env.integration lives at the repo root.
	_ = godotenv.Load("../.env.integration")

	appCfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	cfg := &Config{Config: *appCfg}

	cfg.SourceToken = os.Getenv("E2E_SOURCE_BOT_TOKEN")
	if cfg.SourceToken == "" {
		return nil, fmt.Errorf("E2E_SOURCE_BOT_TOKEN is required")
	}
	cfg.SourceToken2 = os.Getenv("E2E_SOURCE_BOT_TOKEN_2")

	cfg.ListenerToken = os.Getenv("E2E_LISTENER_BOT_TOKEN")
	if cfg.ListenerToken == "" {
		return nil, fmt.Errorf("E2E_LISTENER_BOT_TOKEN is required")
	}

	if cfg.GuildID, err = requireSnowflake("E2E_TEST_GUILD_ID"); err != nil {
		return nil, err
	}
	cfg.GuestGuildID, _ = optionalSnowflake("E2E_GUEST_GUILD_ID")
	if cfg.OwnerChannelID, err = requireSnowflake("E2E_OWNER_CHANNEL_ID"); err != nil {
		return nil, err
	}
	if cfg.Speaker1ChannelID, err = requireSnowflake("E2E_SPEAKER_CHANNEL_ID"); err != nil {
		return nil, err
	}
	cfg.Speaker2ChannelID, _ = optionalSnowflake("E2E_SPEAKER2_CHANNEL_ID")
	if cfg.CallerRoleID, err = requireSnowflake("E2E_CALLER_ROLE_ID"); err != nil {
		return nil, err
	}
	cfg.GuestOwnerChannelID, _ = optionalSnowflake("E2E_GUEST_OWNER_CHANNEL_ID")
	cfg.GuestSpeakerChannelID, _ = optionalSnowflake("E2E_GUEST_SPEAKER_CHANNEL_ID")

	cfg.SamplesDir = os.Getenv("E2E_SAMPLES_DIR")
	if cfg.SamplesDir == "" {
		cfg.SamplesDir = "../integration/samples"
	}

	return cfg, nil
}

func requireSnowflake(env string) (snowflake.ID, error) {
	v := os.Getenv(env)
	if v == "" {
		return 0, fmt.Errorf("%s is required", env)
	}
	id, err := snowflake.Parse(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid snowflake %q: %w", env, v, err)
	}
	return id, nil
}

func optionalSnowflake(env string) (snowflake.ID, error) {
	v := os.Getenv(env)
	if v == "" {
		return 0, nil
	}
	return snowflake.Parse(v)
}
