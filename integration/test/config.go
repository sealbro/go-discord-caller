//go:build integration

package test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
)

type Config struct {
	OwnerToken        string
	SpeakerTokens     []string
	SourceToken       string
	SourceToken2      string // optional; enables E2/E6
	ListenerToken     string
	GuildID           snowflake.ID
	GuestGuildID      snowflake.ID // optional; enables E5
	OwnerChannelID    snowflake.ID
	Speaker1ChannelID snowflake.ID
	Speaker2ChannelID snowflake.ID // optional; enables E2/E6
	CallerRoleID      snowflake.ID
	// Guest guild channels (required only when GuestGuildID is set)
	GuestOwnerChannelID   snowflake.ID
	GuestSpeakerChannelID snowflake.ID
	// Directory containing .dca files streamed in random order by the source bot.
	SamplesDir string
}

func LoadConfig() (*Config, error) {
	// go test sets cwd to the package directory (integration/); .env.integration lives at the repo root.
	_ = godotenv.Load("../.env.integration")

	cfg := &Config{}
	var err error

	cfg.OwnerToken = os.Getenv("DISCORD_OWNER_BOT_TOKEN")
	if cfg.OwnerToken == "" {
		return nil, fmt.Errorf("DISCORD_OWNER_BOT_TOKEN is required")
	}
	cfg.SpeakerTokens = loadSpeakerTokens()

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

var speakerTokenPattern = regexp.MustCompile(`^DISCORD_SPEAKER_BOT_TOKEN_(\d+)$`)

func loadSpeakerTokens() []string {
	type entry struct {
		idx   int
		token string
	}
	var all []entry
	for _, env := range os.Environ() {
		eq := strings.IndexByte(env, '=')
		if eq < 0 {
			continue
		}
		k, v := env[:eq], env[eq+1:]
		if v == "" {
			continue
		}
		m := speakerTokenPattern.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		all = append(all, entry{idx, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].idx < all[j].idx })
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.token
	}
	return out
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
