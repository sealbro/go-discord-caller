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

// countSpeakers reports the total speaker count for telemetry: joined pool
// speaker bots plus the owner bot, if it also joined and is relaying audio.
func countSpeakers(joined int, ownerJoined bool) int {
	if ownerJoined {
		return joined + 1
	}
	return joined
}

// JoinSession connects this guild as a guest to an existing relay session.
// guestMode is the caller-mode chosen by the guest (RaidModeAllyListener or
// RaidModeAllyCaller). When the host does not allow guest capture the mode
// is downgraded to RaidModeAllyListener.
//
// Returns the effective RaidMode (which may differ from the requested mode).
// The session ends automatically when the host ends or ctx is cancelled.
func (m *Service) JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, guestMode guild.RaidMode, code ally.Code) (effectiveMode guild.RaidMode, err error) {
	if !m.starting.tryBegin(guestGuildID) {
		return guestMode, ErrSessionExists
	}
	defer m.starting.end(guestGuildID)

	ctx, span := telemetry.Tracer.Start(ctx, "voice.session.guest",
		trace.WithAttributes(
			attribute.String("guild.id", guestGuildID.String()),
			attribute.String("relay.code", code),
			attribute.String("guest.mode", string(guestMode)),
		),
	)
	// Mirrors the host path: the guest session span lives as long as the raid,
	// so its startup cost gets a span of its own. See StartVoiceRaid.
	setupCtx, endSetup := startPhase(ctx, "voice.session.setup")
	defer func() { endSetup(err) }()

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
	guestGm := m.metrics.ForGuild(ctx, guestGuildID)
	setup, err := m.setupSpeakers(setupCtx, guestGuildID, guestMode, allowUser.Check, guestGm)
	if err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		endSpanErr(span, err)
		return guestMode, err
	}

	// Join the owner bot into its bound channel.
	// In AllyCaller mode the owner also captures incoming audio (WithVoiceReceiver)
	// so users speaking in the owner's channel are relayed to the host — mirroring
	// what StartVoiceRaid does for the host owner bot.
	ownerVoice := m.ownerVoice(guestGuildID)
	var ownerCleanup func()
	var ownerChOut chan []byte
	var ownerHandle *opus.FanoutHandle
	ownerCtx, endOwner := startPhase(setupCtx, "voice.bot.attach",
		botAttr(m.ownerBotID), attribute.String("bot.kind", "owner"))
	openCtx, endOpen := startPhase(ownerCtx, "voice.conn.open", botAttr(m.ownerBotID))
	conn, joinErr := ownerVoice.Join(openCtx, guestGuildID)
	endOpen(joinErr)
	if joinErr != nil {
		endOwner(joinErr)
		slog.WarnContext(ctx, "guest: failed to join owner channel", slog.Any("err", joinErr))
	} else if conn == nil {
		endOwner(nil)
	} else {
		ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceProvider(guestGm.Provider())
		if guestMode.WithCapture() {
			m.prefetchChannelMembers(ownerCtx, conn, m.ownerBotID, guestGuildID)
			ownerSetup.WithVoiceReceiver(allowUser.Check, guestGm.Receiver())
		}
		ownerChOut = make(chan []byte, opus.AudioChanBuf)
		applyCtx, endApply := startPhase(ownerCtx, "voice.conn.apply", botAttr(m.ownerBotID))
		handle, cleanup, err := ownerSetup.Apply(applyCtx, conn, ownerChOut)
		endApply(err)
		if err != nil {
			endOwner(err)
			slog.WarnContext(ctx, "guest: failed to setup owner relay", slog.Any("err", err))
			ownerChOut = nil
		} else {
			ownerCleanup = cleanup
			ownerHandle = handle
			m.storeApplier(guestGuildID, m.ownerBotID, m.buildApplier(guestGuildID, m.ownerBotID, ownerChOut, handle, allowUser.Check))
			m.watchVoiceReady(guestGuildID, m.ownerBotID, conn)
			// Reconcile the owner exactly as joinSpeakers does for speakers.
			// Listener modes build no router (GuestListenerPipeline), so the
			// controller is never told to deafen this bot — it must be handled
			// statically here or it would keep decrypting relay audio it
			// discards. The reconcile also repairs a flag stranded by an
			// earlier session, which nothing else would ever clear for the
			// owner bot.
			ownerCaptures := guestMode.WithCapture()
			deafCtx, endDeaf := startPhase(ownerCtx, "voice.deaf.reconcile",
				botAttr(m.ownerBotID), attribute.Bool("deaf.want", !ownerCaptures))
			ownerUndeafen := m.reconcileSpeakerDeaf(deafCtx, guestGuildID, m.ownerBotID, ownerCaptures, deafControllerOf(setup.Deaf))
			endDeaf(nil)
			endOwner(nil)
			setup.Deaf.Register(m.ownerBotID, !ownerCaptures && ownerUndeafen != nil)
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
	// Build gets the session context, not the phase context: the goroutines it
	// starts outlive every span here.
	_, endBuild := startPhase(setupCtx, "voice.pipeline.build")
	session, start, pipelineCleanup, err := pipeline.GuestFor(guestMode).Build(ctx, params)
	endBuild(err)
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

	speakerCount := countSpeakers(len(setup.Joined), ownerCleanup != nil)
	guestGm.SessionStarted(speakerCount, string(guestMode))
	span.SetAttributes(attribute.Int("speaker.count", speakerCount))
	slog.InfoContext(ctx, "guest joined relay session",
		slog.String("guildID", guestGuildID.String()),
		slog.String("hostMode", string(allySession.HostMode)),
		slog.String("guestMode", string(guestMode)),
		slog.String("code", code),
		slog.Int("activeSpeakers", speakerCount),
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
	return m.stopSession(ctx, guildID, nil)
}

// stopSession tears down the guild's active session. When want is non-nil only
// that exact session is stopped, so a caller holding a reference that went stale
// while it waited cannot tear down the raid that replaced it.
func (m *Service) stopSession(ctx context.Context, guildID snowflake.ID, want *guild.Session) error {
	// Extract and clear the session under write lock; do I/O outside.
	m.mu.Lock()
	status := m.statuses[guildID]
	if status == nil || !status.HasActiveSession() {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	session := status.Session
	if want != nil && session != want {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
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
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (code ally.Code, err error) {
	if !m.starting.tryBegin(guildID) {
		return "", ErrSessionExists
	}
	defer m.starting.end(guildID)

	ctx, span := telemetry.Tracer.Start(ctx, "voice.session",
		trace.WithAttributes(
			attribute.String("guild.id", guildID.String()),
			attribute.String("raid.mode", string(mode)),
		),
	)
	// The session span lives as long as the raid, so the startup cost — the
	// seconds between the command and the first audible frame — needs a span of
	// its own. Its children break that down per Discord round trip.
	setupCtx, endSetup := startPhase(ctx, "voice.session.setup")
	defer func() { endSetup(err) }()

	gm := m.metrics.ForGuild(ctx, guildID)
	allowUser := m.buildAllowUserFilter(guildID)
	setup, err := m.setupSpeakers(setupCtx, guildID, mode, allowUser.Check, gm)
	if err != nil {
		endSpanErr(span, err)
		return "", err
	}
	// Everything below runs only once every speaker is in, so the owner's phases
	// sit end-to-end after the speakers' in the trace rather than beside them.
	ownerCtx, endOwner := startPhase(setupCtx, "voice.bot.attach",
		botAttr(m.ownerBotID), attribute.String("bot.kind", "owner"))
	ov := m.ownerVoice(guildID)
	openCtx, endOpen := startPhase(ownerCtx, "voice.conn.open", botAttr(m.ownerBotID))
	conn, err := ov.Join(openCtx, guildID)
	endOpen(err)
	if err != nil {
		endOwner(err)
		setup.SpeakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: join owner channel: %w", err)
	}
	if conn == nil {
		setup.SpeakerCleanup()
		err = fmt.Errorf("start raid: owner voice connection nil")
		endOwner(err)
		endSpanErr(span, err)
		return "", err
	}
	m.prefetchChannelMembers(ownerCtx, conn, m.ownerBotID, guildID)
	ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceReceiver(allowUser.Check, gm.Receiver())
	// In multi-channel capture modes the owner bot must also play back the
	// mixed audio from other channels into its own channel (mix-minus).
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, opus.AudioChanBuf)
		ownerSetup.WithVoiceProvider(gm.Provider())
	}
	applyCtx, endApply := startPhase(ownerCtx, "voice.conn.apply", botAttr(m.ownerBotID))
	ownerHandle, ownerCleanup, err := ownerSetup.Apply(applyCtx, conn, chOwnerOut)
	endApply(err)
	if err != nil {
		endOwner(err)
		setup.SpeakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: setup owner capture: %w", err)
	}
	m.storeApplier(guildID, m.ownerBotID, m.buildApplier(guildID, m.ownerBotID, chOwnerOut, ownerHandle, allowUser.Check))
	m.watchVoiceReady(guildID, m.ownerBotID, conn)
	// The host owner bot always captures, so it starts hearing; the controller
	// deafens it if its channel ever empties of role-bearing callers. Reconcile
	// rather than assume: a flag stranded by an earlier session would otherwise
	// persist forever here, since nothing else clears the owner's, and the
	// controller would believe it is already hearing and never correct it.
	deafCtx, endDeaf := startPhase(ownerCtx, "voice.deaf.reconcile",
		botAttr(m.ownerBotID), attribute.Bool("deaf.want", false))
	m.reconcileSpeakerDeaf(deafCtx, guildID, m.ownerBotID, true, deafControllerOf(setup.Deaf))
	endDeaf(nil)
	endOwner(nil)
	setup.Deaf.Register(m.ownerBotID, false)
	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)
	// Owner bot always joins the host channel (join failures abort above), so
	// it counts as a speaker alongside the pool bots in setup.Joined.
	speakerCount := countSpeakers(len(setup.Joined), true)
	span.SetAttributes(
		attribute.String("relay.code", allyCode),
		attribute.Int("speaker.count", speakerCount),
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
	// Build gets the session context, not the phase context: the mixer and
	// router goroutines it starts outlive every span here.
	_, endBuild := startPhase(setupCtx, "voice.pipeline.build")
	session, start, err := pipeline.HostFor(mode).Build(ctx, p)
	endBuild(err)
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
	gm.SessionStarted(speakerCount, string(mode))
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
		slog.Int("activeSpeakers", speakerCount),
	)
	start()
	return allyCode, nil
}
