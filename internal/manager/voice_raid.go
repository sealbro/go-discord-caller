package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/pipeline"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// JoinSession connects this guild as a guest to an existing relay session.
// guestMode is the caller-mode chosen by the guest (RaidModeAllyListener or
// RaidModeAllyCaller). When the host does not allow guest capture the mode
// is downgraded to RaidModeAllyListener.
//
// Returns the effective RaidMode (which may differ from the requested mode).
// The session ends automatically when the host ends or ctx is cancelled.
func (m *Service) JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, guestMode guild.RaidMode, code ally.Code) (guild.RaidMode, error) {
	ctx, span := telemetry.Tracer.Start(ctx, "voice.session.guest",
		trace.WithAttributes(
			attribute.String("guild.id", guestGuildID.String()),
			attribute.String("relay.code", code),
			attribute.String("guest.mode", string(guestMode)),
		),
	)
	allySession, err := m.sessions.Join(code, guestGuildID)
	if err != nil {
		endSpanErr(span, err)
		return guestMode, err
	}
	span.SetAttributes(attribute.String("host.mode", string(allySession.HostMode)))
	if guestMode.WithCapture() && !allySession.HostMode.AllowGuestCapture() {
		guestMode = guild.RaidModeAllyListener
	}
	allowUser := m.buildAllowUserFilter(guestGuildID)
	setup, err := m.setupSpeakers(ctx, guestGuildID, guestMode, allowUser.Check)
	if err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		endSpanErr(span, err)
		return guestMode, err
	}
	guestGm := m.metrics.ForGuild(ctx, guestGuildID)

	// Join the owner bot into its bound channel.
	// In AllyCaller mode the owner also captures incoming audio (WithVoiceReceiver)
	// so users speaking in the owner's channel are relayed to the host — mirroring
	// what StartVoiceRaid does for the host owner bot.
	ownerVoice := m.ownerVoice(guestGuildID)
	var ownerCleanup func()
	var ownerChOut chan []byte
	var ownerHandle *opus.FanoutHandle
	if conn, err := ownerVoice.Join(ctx, guestGuildID); err != nil {
		slog.WarnContext(ctx, "guest: failed to join owner channel", slog.Any("err", err))
	} else if conn != nil {
		ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceProvider(guestGm.Provider())
		if guestMode.WithCapture() {
			m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guestGuildID)
			ownerSetup.WithVoiceReceiver(allowUser.Check, guestGm.Receiver())
		}
		ownerChOut = make(chan []byte, opus.AudioChanBuf)
		handle, cleanup, err := ownerSetup.Apply(ctx, conn, ownerChOut)
		if err != nil {
			slog.WarnContext(ctx, "guest: failed to setup owner relay", slog.Any("err", err))
			ownerChOut = nil
		} else {
			ownerCleanup = cleanup
			ownerHandle = handle
			m.storeApplier(guestGuildID, m.ownerBotID, m.buildApplier(guestGuildID, m.ownerBotID, ownerChOut, handle, allowUser.Check))
			m.watchVoiceReady(guestGuildID, m.ownerBotID, conn)
		}
	}
	guestCleanupOwner := func() {
		if ownerCleanup != nil {
			ownerCleanup()
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), voiceLeaveTimeout)
			defer leaveCancel()
			ownerVoice.Leave(leaveCtx, guestGuildID)
		}
	}

	params := pipeline.GuestParams{
		GuestGuildID:   guestGuildID,
		OwnerBotID:     m.ownerBotID,
		OwnerChannelID: ownerVoice.ChannelID(),
		CancelFunc:     cancelFunc,
		Code:           code,
		GuestMode:      guestMode,
		AllySession:    allySession,
		Setup:          setup,
		OwnerChOut:     ownerChOut,
		OwnerHandle:    ownerHandle,
		GuestGM:        guestGm,
		AllowFilter:    allowUser,
		VoiceProbe:     &cacheVoiceProbe{svc: m, guildID: guestGuildID},
	}
	session, start, pipelineCleanup, err := pipeline.GuestFor(guestMode).Build(ctx, params)
	if err != nil {
		setup.SpeakerCleanup()
		guestCleanupOwner()
		m.sessions.RemoveGuest(guestGuildID)
		endSpanErr(span, err)
		return guestMode, err
	}
	if err := m.commitSession(session); err != nil {
		setup.SpeakerCleanup()
		guestCleanupOwner()
		m.sessions.RemoveGuest(guestGuildID)
		endSpanErr(span, err)
		return guestMode, fmt.Errorf("join session: commit: %w", err)
	}
	// Initial pause state (per-cascade + listener check) is seeded by the
	// router's Recompute inside start(); no separate sync pass needed.
	m.startSessionIdleWatcher(ctx, cancelFunc, session)
	start()

	guestGm.SessionStarted(len(setup.Joined), string(guestMode))
	span.SetAttributes(attribute.Int("speaker.count", len(setup.Joined)))
	slog.InfoContext(ctx, "guest joined relay session",
		slog.String("guildID", guestGuildID.String()),
		slog.String("hostMode", string(allySession.HostMode)),
		slog.String("guestMode", string(guestMode)),
		slog.String("code", code),
		slog.Int("activeSpeakers", len(setup.Joined)),
		slog.Bool("ownerRelaying", ownerCleanup != nil),
	)
	go func() {
		defer func() {
			// Clear session first so that voice-leave events fired during cleanup
			// do not trigger a spurious reconnect via ReconnectBotChannel.
			m.mu.Lock()
			if st := m.statuses[guestGuildID]; st != nil {
				st.Session = nil
			}
			m.clearActiveRouter(guestGuildID)
			m.mu.Unlock()
			setup.SpeakerCleanup()
			guestCleanupOwner()
			// Remove from relay BEFORE closing channels to prevent send-on-closed-channel.
			allySession.RemoveGuild(guestGuildID)
			pipelineCleanup()
			m.sessions.RemoveGuest(guestGuildID)
			m.clearAppliers(guestGuildID)
			guestGm.SessionStopped(string(guestMode))
			span.End()
			slog.InfoContext(ctx, "guest session ended", slog.String("guildID", guestGuildID.String()))
		}()
		select {
		case <-ctx.Done():
		case <-allySession.Done():
			cancelFunc()
		}
	}()
	return guestMode, nil
}

// StopVoiceRaid makes all active speakers leave their voice channels.
func (m *Service) StopVoiceRaid(ctx context.Context, guildID snowflake.ID) error {
	// Extract and clear the session under write lock; do I/O outside.
	m.mu.Lock()
	status := m.statuses[guildID]
	if status == nil || !status.HasActiveSession() {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	session := status.Session
	status.Session = nil
	m.clearActiveRouter(guildID)
	m.mu.Unlock()
	session.Cancel()
	if session.Cleanup != nil {
		session.Cleanup()
	}
	if !session.IsGuest {
		m.ownerVoice(guildID).Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
	}
	m.clearAppliers(guildID)
	slog.InfoContext(ctx, "voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
// mode controls which channels capture audio; guests can always join via the relay code.
// Returns the relay session code.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (ally.Code, error) {
	ctx, span := telemetry.Tracer.Start(ctx, "voice.session",
		trace.WithAttributes(
			attribute.String("guild.id", guildID.String()),
			attribute.String("raid.mode", string(mode)),
		),
	)
	allowUser := m.buildAllowUserFilter(guildID)
	setup, err := m.setupSpeakers(ctx, guildID, mode, allowUser.Check)
	if err != nil {
		endSpanErr(span, err)
		return "", err
	}
	ov := m.ownerVoice(guildID)
	conn, err := ov.Join(ctx, guildID)
	if err != nil {
		setup.SpeakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: join owner channel: %w", err)
	}
	if conn == nil {
		setup.SpeakerCleanup()
		err = fmt.Errorf("start raid: owner voice connection nil")
		endSpanErr(span, err)
		return "", err
	}
	m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guildID)
	gm := m.metrics.ForGuild(ctx, guildID)
	ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceReceiver(allowUser.Check, gm.Receiver())
	// In multi-channel capture modes the owner bot must also play back the
	// mixed audio from other channels into its own channel (mix-minus).
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, opus.AudioChanBuf)
		ownerSetup.WithVoiceProvider(gm.Provider())
	}
	ownerHandle, ownerCleanup, err := ownerSetup.Apply(ctx, conn, chOwnerOut)
	if err != nil {
		setup.SpeakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: setup owner capture: %w", err)
	}
	m.storeApplier(guildID, m.ownerBotID, m.buildApplier(guildID, m.ownerBotID, chOwnerOut, ownerHandle, allowUser.Check))
	m.watchVoiceReady(guildID, m.ownerBotID, conn)
	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)
	span.SetAttributes(
		attribute.String("relay.code", allyCode),
		attribute.Int("speaker.count", len(setup.Joined)),
	)
	// errCleanup undoes everything that committed after allySession was created.
	errCleanup := func() {
		setup.SpeakerCleanup()
		ownerCleanup()
		ov.Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
	}
	p := pipeline.Params{
		GuildID:      guildID,
		OwnerBotID:   m.ownerBotID,
		CancelFunc:   cancelFunc,
		Mode:         mode,
		AllyCode:     allyCode,
		AllySession:  allySession,
		Setup:        setup,
		OwnerHandle:  ownerHandle,
		ChOwnerOut:   chOwnerOut,
		OwnerCleanup: ownerCleanup,
		OV:           ov,
		GM:           gm,
		AllowFilter:  allowUser,
		VoiceProbe:   &cacheVoiceProbe{svc: m, guildID: guildID},
	}
	session, start, err := pipeline.HostFor(mode).Build(ctx, p)
	if err != nil {
		errCleanup()
		endSpanErr(span, err)
		return "", err
	}
	if err := m.commitSession(session); err != nil {
		errCleanup()
		endSpanErr(span, err)
		return "", err
	}
	// Initial pause state (per-cascade + listener check) is seeded by the
	// router's Recompute inside start(); no separate sync pass needed.
	m.startSessionIdleWatcher(ctx, cancelFunc, session)
	gm.SessionStarted(len(setup.Joined), string(mode))
	logMsg := "voice raid started"
	if mode.IsDirectPassthrough() {
		logMsg = "voice raid started (direct passthrough)"
	} else if mode.IsStarTopology() {
		logMsg = "voice raid started (star direct)"
	}
	slog.InfoContext(ctx, logMsg,
		slog.String("guildID", guildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", allyCode),
		slog.Int("activeSpeakers", len(setup.Joined)),
	)
	start()
	return allyCode, nil
}
