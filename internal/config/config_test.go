package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func TestParseVoiceLogLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"", telemetry.LevelVoiceOff},
		{"off", telemetry.LevelVoiceOff},
		{"OFF", telemetry.LevelVoiceOff},
		{"none", telemetry.LevelVoiceOff},
		{"nonsense", telemetry.LevelVoiceOff},
		{"warn", slog.LevelWarn},
		{" Error ", slog.LevelError},
		{"debug", slog.LevelDebug},
	}

	for _, tt := range tests {
		if got := parseVoiceLogLevel(tt.in); got != tt.want {
			t.Errorf("parseVoiceLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseSessionIdleTimeout(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", defaultSessionIdleTimeout},
		{"0", 0},
		{"90s", 90 * time.Second},
		{"soon", defaultSessionIdleTimeout},
	}

	for _, tt := range tests {
		if got := parseSessionIdleTimeout(tt.in); got != tt.want {
			t.Errorf("parseSessionIdleTimeout(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
	}

	for _, tt := range tests {
		if got := parseLogLevel(tt.in); got != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestStorePath(t *testing.T) {
	t.Setenv("STORE_PATH", "")
	if got := storePath(); got != "store.yaml" {
		t.Errorf("storePath() with STORE_PATH unset = %q, want store.yaml", got)
	}

	t.Setenv("STORE_PATH", "/data/store.yaml")
	if got := storePath(); got != "/data/store.yaml" {
		t.Errorf("storePath() = %q, want /data/store.yaml", got)
	}
}

func TestLoadRequiresOwnerToken(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DISCORD_OWNER_BOT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load without an owner token must fail")
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DISCORD_OWNER_BOT_TOKEN", "owner")
	t.Setenv("TEST_ALLOW_BOTS", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.OwnerBotToken != "owner" || !cfg.Test.AllowBots {
		t.Errorf("Load = %+v, want owner token and AllowBots", cfg)
	}
}
