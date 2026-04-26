package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
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
	setup, err := m.setupSpeakers(ctx, guestGuildID, guestMode, allowUser)
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
	var ownerChIn chan []byte // non-nil in AllyCaller mode; capture from owner's channel
	if conn, err := ownerVoice.Join(ctx, guestGuildID); err != nil {
		slog.WarnContext(ctx, "guest: failed to join owner channel", slog.Any("err", err))
	} else if conn != nil {
		ownerSetup := NewVoiceConnSetup(m.ownerBotID, m.metrics.Opus.For(guestGuildID.String())).WithVoiceProvider(m.metrics.Session.FrameDropper(ctx, guestGuildID, telemetry.DropPathProvider))
		if guestMode.WithCapture() {
			m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guestGuildID)
			ownerSetup.WithVoiceReceiver(allowUser, m.metrics.Session.FrameDropper(ctx, guestGuildID, telemetry.DropPathReceiver))
		}
		ownerChOut = make(chan []byte, audioChanBuf)
		chIn, cleanup, err := ownerSetup.Apply(ctx, conn, ownerChOut)
		if err != nil {
			slog.WarnContext(ctx, "guest: failed to setup owner relay", slog.Any("err", err))
			ownerChOut = nil
		} else {
			ownerCleanup = cleanup
			ownerChIn = chIn
			m.storeApplier(guestGuildID, m.ownerBotID, m.buildOwnerApplier(guestGuildID, ownerChIn, ownerChOut, allowUser))
		}
	}
	// guestCleanupOwner consolidates owner teardown used in both error paths and deferred teardown.
	guestCleanupOwner := func() {
		if ownerCleanup != nil {
			ownerCleanup()
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), voiceLeaveTimeout)
			defer leaveCancel()
			ownerVoice.Leave(leaveCtx, guestGuildID)
		}
	}
	// In AllyCaller mode build a full mix-minus graph identical to the host:
	// one ChannelMixer per destination channel (mix-minus), a relay mixer whose
	// output is broadcast to other guilds, and per-channel relay inputs so the
	// host relay reaches guest speakers through the mixers.
	//
	// In AllyListener mode register raw chOut channels directly — no local mixing.
	var (
		guestChannelMixers map[snowflake.ID]*opus.Mixer
		guestRelayMixer    *opus.Mixer
		destinations       []*destChannel
	)
	if guestMode.WithCapture() {
		destinations = buildDestinations(setup.joined)
		if ownerChOut != nil {
			destinations = append(destinations, &destChannel{
				channelID: ownerVoice.ChannelID(),
				outs:      []chan<- []byte{ownerChOut},
			})
		}
		// Star topology with direct output: channel mixers are not needed — host relay
		// delivers raw Opus bytes directly to speaker chOuts. Only the relay mixer is
		// created (for capture → relay direction).
		if !guestMode.IsDirectOutput() {
			guestChannelMixers = make(map[snowflake.ID]*opus.Mixer, len(destinations))
			for _, dest := range destinations {
				mx, err := opus.NewMixer(m.metrics.Opus.For(guestGuildID.String()))
				if err != nil {
					setup.speakerCleanup()
					guestCleanupOwner()
					m.sessions.RemoveGuest(guestGuildID)
					endSpanErr(span, err)
					return guestMode, fmt.Errorf("guest: create channel mixer: %w", err)
				}
				guestChannelMixers[dest.channelID] = mx
			}
		}
		guestRelayMixer, err = opus.NewMixer(m.metrics.Opus.For(guestGuildID.String()))
		if err != nil {
			setup.speakerCleanup()
			guestCleanupOwner()
			m.sessions.RemoveGuest(guestGuildID)
			endSpanErr(span, err)
			return guestMode, fmt.Errorf("guest: create relay mixer: %w", err)
		}
	}
	var guestMixerPausers map[snowflake.ID]guild.MixerPauser
	if guestChannelMixers != nil {
		guestMixerPausers = make(map[snowflake.ID]guild.MixerPauser, len(guestChannelMixers))
		for chID, mx := range guestChannelMixers {
			guestMixerPausers[chID] = mx
		}
	}
	session := &guild.Session{
		GuildID:       guestGuildID,
		Cancel:        cancelFunc,
		Cleanup:       setup.speakerCleanup,
		AllyCode:      code,
		IsGuest:       true,
		Speakers:      setup.speakers,
		ChannelMixers: guestMixerPausers,
	}
	if err := m.commitSession(session); err != nil {
		setup.speakerCleanup()
		guestCleanupOwner()
		m.sessions.RemoveGuest(guestGuildID)
		endSpanErr(span, err)
		return guestMode, fmt.Errorf("join session: commit: %w", err)
	}
	// Pause mixers for channels that currently have no non-bot listeners.
	if guestMixerPausers != nil {
		m.syncMixerPauseState(guestGuildID, session)
	}
	// toClose holds channels closed on teardown. In caller mode these are the
	// relay input channels (speaker chOuts are closed by channel mixer goroutines).
	// In listener mode these are the raw speaker/owner chOut channels.
	var toClose []chan<- []byte
	if guestMode.WithCapture() && guestRelayMixer != nil {
		sources := buildGuestSources(ctx, setup.joined)
		// Include the owner bot's capture channel as a source so users speaking
		// in the owner's channel are mixed and relayed — the missing half of the
		// guest AllyCaller flow that speakers already provide for their channels.
		if ownerChIn != nil {
			sources = append(sources, sourceEntry{m.ownerBotID, ownerVoice.ChannelID(), ownerChIn})
		}
		if guestMode.IsStarTopology() {
			// Guest star: all sources → relay only (no local channel routing).
			wireFanoutOneMany(ctx, guestGuildID, sources, destinations, guestChannelMixers, guestRelayMixer, 0, &m.metrics.Session)
			// Output: deliver host relay directly to speaker chOuts — no channel
			// mixers needed. Each guest channel mixer has exactly one input (relay)
			// and would just passthrough unchanged, so bypass them entirely.
			allOuts := make([]chan<- []byte, len(setup.outs))
			copy(allOuts, setup.outs)
			if ownerChOut != nil {
				allOuts = append(allOuts, ownerChOut)
			}
			allySession.AddGuild(guestGuildID, allOuts)
			toClose = allOuts
			startGuestRelayBroadcast(ctx, guestRelayMixer, allySession, guestGuildID)
		} else {
			wireFanout(ctx, guestGuildID, sources, destinations, guestChannelMixers, guestRelayMixer, &m.metrics.Session)
			toClose = registerRelayInputs(ctx, guestGuildID, allySession, destinations, guestChannelMixers, &m.metrics.Session)
			startChannelMixers(ctx, destinations, guestChannelMixers, guestGuildID, &m.metrics.Session)
			startGuestRelayBroadcast(ctx, guestRelayMixer, allySession, guestGuildID)
		}
	} else {
		outs := make([]chan<- []byte, len(setup.outs))
		copy(outs, setup.outs)
		if ownerChOut != nil {
			outs = append(outs, ownerChOut)
		}
		allySession.AddGuild(guestGuildID, outs)
		toClose = outs
	}
	m.metrics.Session.SessionStarted(ctx, guestGuildID, len(setup.joined))
	span.SetAttributes(attribute.Int("speaker.count", len(setup.joined)))
	slog.InfoContext(ctx, "guest joined relay session",
		slog.String("guildID", guestGuildID.String()),
		slog.String("hostMode", string(allySession.HostMode)),
		slog.String("guestMode", string(guestMode)),
		slog.String("code", code),
		slog.Int("activeSpeakers", len(setup.joined)),
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
			m.mu.Unlock()
			setup.speakerCleanup()
			guestCleanupOwner()
			// Remove from relay BEFORE closing channels to prevent send-on-closed-channel.
			allySession.RemoveGuild(guestGuildID)
			for _, ch := range toClose {
				close(ch)
			}
			m.sessions.RemoveGuest(guestGuildID)
			m.clearAppliers(guestGuildID)
			m.metrics.Session.SessionStopped(ctx, guestGuildID)
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
	setup, err := m.setupSpeakers(ctx, guildID, mode, allowUser)
	if err != nil {
		endSpanErr(span, err)
		return "", err
	}
	ov := m.ownerVoice(guildID)
	conn, err := ov.Join(ctx, guildID)
	if err != nil {
		setup.speakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: join owner channel: %w", err)
	}
	if conn == nil {
		setup.speakerCleanup()
		err = fmt.Errorf("start raid: owner voice connection nil")
		endSpanErr(span, err)
		return "", err
	}
	m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guildID)
	ownerSetup := NewVoiceConnSetup(m.ownerBotID, m.metrics.Opus.For(guildID.String())).WithVoiceReceiver(allowUser, m.metrics.Session.FrameDropper(ctx, guildID, telemetry.DropPathReceiver))
	// In multi-channel capture modes the owner bot must also play back the
	// mixed audio from other channels into its own channel (mix-minus).
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, audioChanBuf)
		ownerSetup.WithVoiceProvider(m.metrics.Session.FrameDropper(ctx, guildID, telemetry.DropPathProvider))
	}
	chIn, ownerCleanup, err := ownerSetup.Apply(ctx, conn, chOwnerOut)
	if err != nil {
		setup.speakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("start raid: setup owner capture: %w", err)
	}
	m.storeApplier(guildID, m.ownerBotID, m.buildOwnerApplier(guildID, chIn, chOwnerOut, allowUser))
	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)
	span.SetAttributes(
		attribute.String("relay.code", allyCode),
		attribute.Int("speaker.count", len(setup.joined)),
	)
	// errCleanup undoes everything that committed after allySession was created.
	errCleanup := func() {
		setup.speakerCleanup()
		ownerCleanup()
		ov.Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
	}
	p := pipelineParams{
		guildID:      guildID,
		ownerBotID:   m.ownerBotID,
		cancelFunc:   cancelFunc,
		mode:         mode,
		allyCode:     allyCode,
		allySession:  allySession,
		setup:        setup,
		chIn:         chIn,
		chOwnerOut:   chOwnerOut,
		ownerCleanup: ownerCleanup,
		ov:           ov,
		metrics:      m.metrics,
	}
	session, start, err := pipelineFor(mode).build(ctx, p)
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
	if !mode.IsDirectPassthrough() {
		m.syncMixerPauseState(guildID, session)
	}
	m.metrics.Session.SessionStarted(ctx, guildID, len(setup.joined))
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
		slog.Int("activeSpeakers", len(setup.joined)),
	)
	start()
	return allyCode, nil
}
