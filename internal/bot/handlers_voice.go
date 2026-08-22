package bot

import (
	"context"
	"errors"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
	"go.opentelemetry.io/otel/trace"
)

// handleSetup opens the interactive setup panel as an ephemeral message.
// Blocked while a voice raid is active. Authorization handled by withAdmin middleware.
func (h *CommandHandlers) handleSetup(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if h.manager.HasActiveSession(guildID) {
		return e.CreateMessage(ephemeral(loc.T("setup.blocked_active_raid")))
	}

	msg, components := h.buildMainSetupMessage(guildID, loc)
	return e.CreateMessage(discord.MessageCreate{
		Content:    msg,
		Components: components,
		Flags:      discord.MessageFlagEphemeral,
	})
}

// handleStartVoiceRaid starts a new voice raid or joins an existing one as a guest.
// Uses deferred responses so the user gets real feedback on success/failure.
// Authorization handled by withManager middleware.
func (h *CommandHandlers) handleStartVoiceRaid(guildID snowflake.ID, loc *i18n.Localizer, data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	status := h.manager.GetStatus(guildID)
	if status.HasActiveSession() {
		return e.CreateMessage(ephemeral(loc.T("raid.already_active")))
	}

	code, hasCode := data.OptString("code")

	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}

	modeStr, _ := data.OptString("mode")

	if hasCode && code != "" {
		var mode guild.RaidMode
		switch modeStr {
		case callerModeMany:
			mode = guild.RaidModeAllyCaller
		case callerModeOneMany:
			mode = guild.RaidModeOneManyAllyCaller
		default:
			mode = guild.RaidModeAllyListener
		}
		cmdCtx := e.Ctx
		ctx, cancelFunc := context.WithCancel(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(cmdCtx)))
		go func() {
			if warnings := h.manager.CheckGuildChannelAccess(guildID); len(warnings) > 0 {
				cancelFunc()
				h.followUp(e, loc.T("raid.join_blocked_permissions")+formatAccessWarnings(loc, warnings))
				return
			}
			effectiveMode, err := h.manager.JoinSession(ctx, guildID, cancelFunc, mode, code)
			if err != nil {
				cancelFunc()
				slog.WarnContext(cmdCtx, "failed to join relay session",
					slog.String("guildID", guildID.String()), slog.String("code", code), slog.Any("err", err))
				if errors.Is(err, manager.ErrNoBoundSpeakers) {
					h.followUp(e, loc.T("raid.no_bound_speakers"))
					return
				}
				h.followUp(e, loc.T("raid.join_failed", "Code", code, "Err", err.Error()))
				return
			}
			h.followUp(e, loc.T("raid.joined", "Code", code, "Mode", effectiveMode.Pretty(loc)))
		}()
		return nil
	}

	var mode guild.RaidMode
	switch modeStr {
	case callerModeMany:
		mode = guild.RaidModeGuildCaller
	case callerModeOneMany:
		mode = guild.RaidModeOneManyGuildCaller
	default:
		mode = guild.RaidModeOneCaller
	}

	cmdCtx := e.Ctx
	ctx, cancelFunc := context.WithCancel(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(cmdCtx)))
	go func() {
		if warnings := h.manager.CheckGuildChannelAccess(guildID); len(warnings) > 0 {
			cancelFunc()
			h.followUp(e, loc.T("raid.start_blocked_permissions")+formatAccessWarnings(loc, warnings))
			return
		}
		relayCode, err := h.manager.StartVoiceRaid(ctx, guildID, cancelFunc, mode)
		if err != nil {
			cancelFunc()
			slog.WarnContext(cmdCtx, "failed to start voice raid",
				slog.String("guildID", guildID.String()), slog.Any("err", err))
			if errors.Is(err, manager.ErrNoBoundSpeakers) {
				h.followUp(e, loc.T("raid.no_bound_speakers"))
				return
			}
			h.followUp(e, loc.T("raid.start_failed", "Err", err.Error()))
			return
		}
		var msg string
		if relayCode != "" {
			msg = loc.T("raid.started_with_code", "Code", relayCode)
		} else {
			msg = loc.T("raid.started")
		}
		slog.InfoContext(cmdCtx, "voice raid started", slog.String("relayCode", relayCode))
		h.followUp(e, msg)
	}()

	return nil
}

// handleStopVoiceRaid stops the active voice raid.
// Uses a deferred response so the user gets real feedback on success/failure.
// Authorization handled by withManager middleware.
func (h *CommandHandlers) handleStopVoiceRaid(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if status := h.manager.GetStatus(guildID); !status.HasActiveSession() {
		return e.CreateMessage(ephemeral(loc.T("raid.none_active")))
	}

	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}

	cmdCtx := e.Ctx
	go func() {
		if err := h.manager.StopVoiceRaid(cmdCtx, guildID); err != nil {
			slog.WarnContext(cmdCtx, "failed to stop voice raid", slog.String("guildID", guildID.String()), slog.Any("err", err))
			h.followUp(e, loc.T("raid.stop_failed", "Err", err.Error()))
			return
		}
		h.followUp(e, loc.T("raid.stopped"))
	}()

	return nil
}

// handleStatus responds with an ephemeral snapshot of the guild's configuration and
// session state. Authorization handled by withGuild middleware.
func (h *CommandHandlers) handleStatus(guildID snowflake.ID, loc *i18n.Localizer, _ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	status := h.manager.GetStatus(guildID)
	return e.CreateMessage(discord.MessageCreate{
		Content: status.Render(loc),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// followUp sends an ephemeral follow-up message after a deferred response.
func (h *CommandHandlers) followUp(e *handler.CommandEvent, content string) {
	if _, err := e.CreateFollowupMessage(ephemeral(content)); err != nil {
		slog.Warn("failed to send follow-up message", slog.Any("err", err))
	}
}
