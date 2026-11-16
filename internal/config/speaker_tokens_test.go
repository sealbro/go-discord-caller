package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func clearSpeakerEnv(t *testing.T) {
	t.Helper()
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if speakerTokenPattern.MatchString(key) {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}
}

func TestLoadSpeakerTokensOrderDoesNotDependOnEnvironmentOrder(t *testing.T) {
	clearSpeakerEnv(t)
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_1", "token-a")
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_01", "token-b")
	first := loadSpeakerTokens()

	clearSpeakerEnv(t)
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_01", "token-b")
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_1", "token-a")
	second := loadSpeakerTokens()

	if !slices.Equal(first, second) {
		t.Errorf("speaker order depends on environment order: %v vs %v", first, second)
	}
}

func TestLoadSpeakerTokensSkipsIndexThatOverflows(t *testing.T) {
	clearSpeakerEnv(t)
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_1", "token-a")
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_99999999999999999999", "token-b")

	if got := loadSpeakerTokens(); !slices.Equal(got, []string{"token-a"}) {
		t.Errorf("loadSpeakerTokens = %v, want only the token with a valid index", got)
	}
}

func TestLoadSpeakerTokensDropsDuplicateTokens(t *testing.T) {
	clearSpeakerEnv(t)
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_1", "token-a")
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_2", "token-a")
	t.Setenv("DISCORD_SPEAKER_BOT_TOKEN_3", "token-b")

	if got := loadSpeakerTokens(); !slices.Equal(got, []string{"token-a", "token-b"}) {
		t.Errorf("loadSpeakerTokens = %v, want each token once", got)
	}
}
