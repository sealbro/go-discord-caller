package telemetry

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// captureVoiceLogger swaps the default slog handler for a buffer and returns a
// VoiceLogger writing into it, so the attributes that reach the OTLP pipeline
// can be asserted directly.
func captureVoiceLogger(t *testing.T, botUserID snowflake.ID, level slog.Level) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	prevLevel := voiceLogLevel.Level()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	SetVoiceLogLevel(level)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		SetVoiceLogLevel(prevLevel)
	})

	return VoiceLogger(botUserID), &buf
}

// Every line disgo emits from the voice layer is otherwise identical across the
// owner bot and all speaker bots, which made a "missing key ratchet" burst
// impossible to attribute to a connection.
func TestVoiceLoggerTagsBotUserID(t *testing.T) {
	logger, buf := captureVoiceLogger(t, snowflake.ID(1484911601210495038), slog.LevelWarn)

	logger.Warn("failed to send audio")

	out := buf.String()
	if !strings.Contains(out, "botUserID=1484911601210495038") {
		t.Errorf("voice log line is missing botUserID, cannot be attributed to a bot: %q", out)
	}
	if !strings.Contains(out, "component=voice") {
		t.Errorf("voice log line is missing component=voice: %q", out)
	}
}

func TestVoiceLoggerOmitsUnknownBotUserID(t *testing.T) {
	logger, buf := captureVoiceLogger(t, 0, slog.LevelWarn)

	logger.Warn("failed to send audio")

	out := buf.String()
	if strings.Contains(out, "botUserID") {
		t.Errorf("unknown bot ID should be omitted, not logged as 0: %q", out)
	}
	if !strings.Contains(out, "component=voice") {
		t.Errorf("voice log line is missing component=voice: %q", out)
	}
}

// Below warn, disgo logs a line per VoiceStateUpdate.
func TestVoiceLoggerDropsBelowWarn(t *testing.T) {
	logger, buf := captureVoiceLogger(t, snowflake.ID(1484911601210495038), slog.LevelWarn)

	logger.Info("voice state update")

	if buf.Len() != 0 {
		t.Errorf("expected Info to be gated out, got %q", buf.String())
	}
}

// disgo logs voice failures per packet, so a single undecryptable connection
// puts tens of thousands of identical lines an hour on the OTLP pipeline.
// Nothing should reach it unless VOICE_LOG_LEVEL was set.
func TestVoiceLoggerSilentByDefault(t *testing.T) {
	logger, buf := captureVoiceLogger(t, snowflake.ID(1484911601210495038), LevelVoiceOff)

	logger.Error("error while reading packet")
	logger.Warn("failed to send audio")

	if buf.Len() != 0 {
		t.Errorf("voice logs must be off by default, got %q", buf.String())
	}
}
