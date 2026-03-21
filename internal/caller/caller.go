package caller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// Caller handles the core voice-call logic.
type Caller struct {
	client *bot.Client
}

// New creates a new Caller.
func New(client *bot.Client) *Caller {
	return &Caller{client: client}
}

// JoinChannel makes the bot join a voice channel.
func (c *Caller) JoinChannel(ctx context.Context, guildID, channelID snowflake.ID) error {
	conn := c.client.VoiceManager.CreateConn(guildID)

	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return err
	}

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking: %w", err)
	}

	slog.Info("joined voice channel",
		slog.String("channelID", channelID.String()),
		slog.String("guildID", guildID.String()),
	)

	return nil
}

// LeaveChannel makes the bot leave the voice channel in a guild.
func (c *Caller) LeaveChannel(ctx context.Context, guildID snowflake.ID) error {
	conn := c.client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return nil
	}

	conn.Close(ctx)

	slog.Info("left voice channel", slog.String("guildID", guildID.String()))

	return nil
}
