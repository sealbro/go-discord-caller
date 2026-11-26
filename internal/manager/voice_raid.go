package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/pipeline"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
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

// leaveWithoutCancel makes gv leave guildID on a context that outlives ctx: a
// cancelled one makes disgo drop the leave, and the bot stays in the channel.
func leaveWithoutCancel(ctx context.Context, gv pool.GuildVoice, guildID snowflake.ID) {
	leaveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceLeaveTimeout)
	defer cancel()
	gv.Leave(leaveCtx, guildID)
}

// JoinSession connects this guild as a guest to an existing relay session.
// guestMode is the caller-mode chosen by the guest (RaidModeAllyListener or
// RaidModeAllyCaller). When the host does not allow guest capture the mode
// is downgraded to RaidModeAllyListener.
//
// Returns the effective RaidMode (which may differ from the requested mode).
// The session ends automatically when the host ends or ctx is cancelled.
func (m *Service) JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, guestMode guild.RaidMode, code ally.Code) (effectiveMode guild.RaidMode, err error) {
	raidStart := time.Now()
	if !m.starting.tryBegin(guestGuildID, cancelFunc) {
		return guestMode, ErrSessionExists
	}
	defer m.starting.end(guestGuildID)
	if m.CheckDeafenReadiness(guestGuildID).MissingPermission {
		return guestMode, ErrNoDeafenPermission
	}

	ctx, span := telemetry.Tracer.Start(ctx, "voice.session.guest",
		trace.WithAttributes(
			attribute.String("guild.id", guestGuildID.String()),
			attribute.String("guest.mode", string(guestMode)),
		),
	)
	// Mirrors the host path: the guest session span lives as long as the raid,
	// so its startup cost gets a span of its own. See StartVoiceRaid.
	setupCtx, endSetup := startPhase(ctx, "voice.session.setup")
	defer func() { endSetup(err) }()
	var undo startRollback
	defer func() {
		if err != nil {
			undo.run()
			endSpanErr(span, err)
		}
	}()

	allySession, err := m.sessions.Join(code, guestGuildID)
	if err != nil {
		return guestMode, err
	}
	undo.relay = func() { m.sessions.RemoveGuest(guestGuildID, allySession) }
	span.SetAttributes(attribute.String("host.mode", string(allySession.HostMode)))
	if guestMode.WithCapture() && !allySession.HostMode.AllowGuestCapture() {
		guestMode = guild.RaidModeAllyListener
	}
	if guestMode.WithCapture() && !m.hasBoundCallerRole(guestGuildID) {
		return guestMode, ErrNoCallerRole
	}
	allowUser := m.buildAllowUserFilter(guestGuildID)
	defer m.trackStartupFilter(guestGuildID, allowUser)()
	guestGm := m.metrics.ForGuild(ctx, guestGuildID)
	undo.reconnect = func() { m.clearReconnectState(guestGuildID) }
	setup, err := m.setupSpeakers(setupCtx, guestGuildID, guestMode, allowUser.Admit, guestGm, raidStart)
	if err != nil {
		return guestMode, err
	}
	undo.audio = setup.SpeakerCleanup

	// Join the owner bot into its bound channel.
	// In AllyCaller mode the owner also captures incoming audio (WithVoiceReceiver)
	// so users speaking in the owner's channel are relayed to the host — mirroring
	// what StartVoiceRaid does for the host owner bot.
	ownerVoice := m.ownerVoice(guestGuildID)
	var ownerCleanup func()
	var ownerChOut chan []byte
	var ownerHandle *opus.FanoutHandle
	ownerJoined := false
	guestCleanupOwner := func() {
		if ownerCleanup != nil {
			ownerCleanup()
		}
		if ownerJoined {
			leaveWithoutCancel(ctx, ownerVoice, guestGuildID)
		}
	}
	undo.owner = guestCleanupOwner
	ownerCtx, endOwner := startPhase(setupCtx, "voice.bot.attach",
		botAttr(m.ownerBotID), attribute.String("bot.kind", "owner"))
	openCtx, endOpen := startPhase(ownerCtx, "voice.conn.open", botAttr(m.ownerBotID))
	conn, joinErr := ownerVoice.Join(openCtx, guestGuildID)
	endOpen(joinErr)
	if ownerVoice.ChannelID() != 0 {
		guestGm.BotJoined("owner", time.Since(raidStart), joinErr)
	}
	if joinErr != nil {
		endOwner(joinErr)
		slog.WarnContext(ctx, "guest: failed to join owner channel", slog.Any("err", joinErr))
	} else if conn == nil {
		endOwner(nil)
	} else {
		ownerJoined = true
		ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceProvider(guestGm.Provider())
		if guestMode.WithCapture() {
			m.prefetchChannelMembers(ownerCtx, conn, m.ownerBotID, guestGuildID)
			ownerSetup.WithVoiceReceiver(allowUser.Admit, guestGm.Receiver())
		}
		ownerChOut = make(chan []byte, opus.AudioChanBuf)
		applyCtx, endApply := startPhase(ownerCtx, "voice.conn.apply", botAttr(m.ownerBotID))
		handle, cleanup, err := ownerSetup.Apply(applyCtx, conn, ownerChOut)
		endApply(err)
		if err != nil {
			endOwner(err)
			slog.WarnContext(ctx, "guest: failed to setup owner relay", slog.Any("err", err))
			return guestMode, fmt.Errorf("join session: setup owner relay: %w", err)
		} else {
			ownerCleanup = cleanup
			ownerHandle = handle
			m.storeApplier(guestGuildID, m.ownerBotID, m.buildApplier(guestGuildID, m.ownerBotID, ownerChOut, handle, allowUser.Admit))
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
			ownerDeafened := m.reconcileSpeakerDeaf(deafCtx, guestGuildID, m.ownerBotID, ownerCaptures, deafControllerOf(setup.Deaf))
			endDeaf(nil)
			endOwner(nil)
			setup.Deaf.Register(m.ownerBotID, ownerDeafened)
		}
	}
	// A guest carries on without its owner, so an owner join failed by a /stop
	// would otherwise commit a session that is already over and report success.
	if err := ctx.Err(); err != nil {
		return guestMode, fmt.Errorf("join session: %w", err)
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
		return guestMode, err
	}
	session.Mode = guestMode
	if session.Cleanup != nil {
		undo.audio = session.Cleanup
	}
	undo.pipeline = pipelineCleanup
	m.starting.hold(guestGuildID)
	if err := m.commitSession(session); err != nil {
		m.starting.release(guestGuildID)
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
		slog.Int("activeSpeakers", speakerCount),
		slog.Int("speakersJoined", len(setup.Joined)),
		slog.Int("speakersCandidates", setup.Candidates),
		slog.Bool("ownerRelaying", ownerCleanup != nil),
	)
	go func() {
		defer func() {
			defer m.starting.release(guestGuildID)
			// Clear session first so that voice-leave events fired during cleanup
			// do not trigger a spurious reconnect via ReconnectBotChannel.
			m.mu.Lock()
			if st := m.statuses[guestGuildID]; st != nil && st.Session == session {
				st.Session = nil
				m.clearActiveRouter(guestGuildID)
			}
			m.mu.Unlock()
			session.Cleanup()
			guestCleanupOwner()
			// Remove from relay BEFORE closing channels to prevent send-on-closed-channel.
			allySession.RemoveGuild(guestGuildID)
			pipelineCleanup()
			m.sessions.RemoveGuest(guestGuildID, allySession)
			m.clearReconnectState(guestGuildID)
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

// StopVoiceRaid makes all active speakers leave their voice channels, or
// aborts the raid still starting when there is no committed session yet.
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
		// A raid is committed only once every bot is in, so during the whole
		// startup there is nothing here to stop. Cancelling that start aborts
		// it before it commits and its own cleanup releases whatever it had
		// already joined. A stop aimed at one specific session (the idle
		// watcher) never does this: the start it would abort is by definition
		// not the session it was watching.
		if want == nil && m.starting.cancel(guildID) {
			return nil
		}
		return ErrNoActiveSession
	}
	session := status.Session
	if want != nil && session != want {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	if session.IsGuest && m.starting.held(guildID) {
		m.mu.Unlock()
		// The guest's own goroutine owns its teardown; running it twice would
		// race a start admitted in between.
		session.Cancel()
		if err := m.starting.awaitReleased(ctx, guildID); err != nil {
			return err
		}
		slog.InfoContext(ctx, "voice raid stopped", slog.String("guildID", guildID.String()))
		return nil
	}
	status.Session = nil
	m.clearActiveRouter(guildID)
	m.starting.hold(guildID)
	m.mu.Unlock()
	defer m.starting.release(guildID)
	session.Cancel()
	if session.Cleanup != nil {
		session.Cleanup()
	}
	if !session.IsGuest {
		m.ownerVoice(guildID).Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
	}
	m.clearReconnectState(guildID)
	slog.InfoContext(ctx, "voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
// mode controls which channels capture audio; guests can always join via the relay code.
// Returns the relay session code.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (code ally.Code, err error) {
	raidStart := time.Now()
	if !m.starting.tryBegin(guildID, cancelFunc) {
		return "", ErrSessionExists
	}
	defer m.starting.end(guildID)
	if m.CheckDeafenReadiness(guildID).MissingPermission {
		return "", ErrNoDeafenPermission
	}
	if !m.hasBoundCallerRole(guildID) {
		return "", ErrNoCallerRole
	}

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
	var undo startRollback
	defer func() {
		if err != nil {
			undo.run()
			endSpanErr(span, err)
		}
	}()

	if _, ok := m.liveBoundChannel(guildID, m.ownerBotID); !ok {
		return "", ErrNoOwnerChannel
	}

	gm := m.metrics.ForGuild(ctx, guildID)
	allowUser := m.buildAllowUserFilter(guildID)
	defer m.trackStartupFilter(guildID, allowUser)()
	undo.reconnect = func() { m.clearReconnectState(guildID) }
	setup, err := m.setupSpeakers(setupCtx, guildID, mode, allowUser.Admit, gm, raidStart)
	if err != nil {
		return "", err
	}
	undo.audio = setup.SpeakerCleanup
	// Everything below runs only once every speaker is in, so the owner's phases
	// sit end-to-end after the speakers' in the trace rather than beside them.
	ownerCtx, endOwner := startPhase(setupCtx, "voice.bot.attach",
		botAttr(m.ownerBotID), attribute.String("bot.kind", "owner"))
	ov := m.ownerVoice(guildID)
	openCtx, endOpen := startPhase(ownerCtx, "voice.conn.open", botAttr(m.ownerBotID))
	conn, err := ov.Join(openCtx, guildID)
	endOpen(err)
	gm.BotJoined("owner", time.Since(raidStart), err)
	if err != nil {
		endOwner(err)
		return "", fmt.Errorf("start raid: join owner channel: %w", err)
	}
	if conn == nil {
		err = fmt.Errorf("start raid: owner voice connection nil")
		endOwner(err)
		return "", err
	}
	undo.leaveOwner = func() { leaveWithoutCancel(ctx, ov, guildID) }
	m.prefetchChannelMembers(ownerCtx, conn, m.ownerBotID, guildID)
	ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceReceiver(allowUser.Admit, gm.Receiver())
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
		return "", fmt.Errorf("start raid: setup owner capture: %w", err)
	}
	undo.owner = ownerCleanup
	m.storeApplier(guildID, m.ownerBotID, m.buildApplier(guildID, m.ownerBotID, chOwnerOut, ownerHandle, allowUser.Admit))
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
	allySession, err := m.sessions.Open(allyCode, guildID, mode)
	if err != nil {
		return "", fmt.Errorf("start raid: register relay session: %w", err)
	}
	undo.relay = func() { m.sessions.Remove(allySession) }
	// Owner bot always joins the host channel (join failures abort above), so
	// it counts as a speaker alongside the pool bots in setup.Joined.
	speakerCount := countSpeakers(len(setup.Joined), true)
	span.SetAttributes(attribute.Int("speaker.count", speakerCount))
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
		return "", err
	}
	session.Mode = mode
	if session.Cleanup != nil {
		undo.audio = session.Cleanup
	}
	// A /stop during startup cancels this context. Committing after that would
	// leave the operator's stop undone: nothing watches ctx on the host path
	// once the session is committed, so the raid would simply stay up. (The
	// guest path needs no such check — the goroutine it starts right after its
	// own commit tears the session down on ctx.Done.)
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err := m.commitSession(session); err != nil {
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
		slog.Int("activeSpeakers", speakerCount),
		slog.Int("speakersJoined", len(setup.Joined)),
		slog.Int("speakersCandidates", setup.Candidates),
	)
	start()
	return allyCode, nil
}
