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
		ownerSetup := NewVoiceConnSetup(m.ownerBotID).WithVoiceProvider()
		if guestMode.WithCapture() {
			m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guestGuildID)
			ownerSetup.WithVoiceReceiver(allowUser)
		}
		ownerChOut = make(chan []byte, audioChanBuf)
		chIn, cleanup, err := ownerSetup.Apply(ctx, conn, ownerChOut)
		if err != nil {
			slog.WarnContext(ctx, "guest: failed to setup owner relay", slog.Any("err", err))
			ownerChOut = nil
		} else {
			ownerCleanup = cleanup
			ownerChIn = chIn
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
				mx, err := opus.NewMixer(&m.metrics.Mixer)
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
		guestRelayMixer, err = opus.NewMixer(&m.metrics.Mixer)
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
		return guestMode, fmt.Errorf("failed to commit session: %w", err)
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
			startChannelMixers(ctx, destinations, guestChannelMixers)
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
			setup.speakerCleanup()
			guestCleanupOwner()
			// Remove from relay BEFORE closing channels to prevent send-on-closed-channel.
			allySession.RemoveGuild(guestGuildID)
			for _, ch := range toClose {
				close(ch)
			}
			m.sessions.RemoveGuest(guestGuildID)
			m.mu.Lock()
			if st := m.statuses[guestGuildID]; st != nil {
				st.Session = nil
			}
			m.mu.Unlock()
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
		return fmt.Errorf("no active voice raid in this server")
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
		return "", fmt.Errorf("failed to join owner channel: %w", err)
	}
	if conn == nil {
		setup.speakerCleanup()
		err = fmt.Errorf("no voice connection to owner channel")
		endSpanErr(span, err)
		return "", err
	}
	m.prefetchChannelMembers(ctx, conn, m.ownerBotID, guildID)
	ownerSetup := NewVoiceConnSetup(m.ownerBotID).
		WithVoiceReceiver(allowUser)
	// In multi-channel capture modes the owner bot must also play back the
	// mixed audio from other channels into its own channel (mix-minus).
	var chOwnerOut chan []byte
	if mode.WithCapture() {
		chOwnerOut = make(chan []byte, audioChanBuf)
		ownerSetup.WithVoiceProvider()
	}
	chIn, ownerCleanup, err := ownerSetup.Apply(ctx, conn, chOwnerOut)
	if err != nil {
		setup.speakerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("failed to setup owner capture: %w", err)
	}
	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)
	span.SetAttributes(
		attribute.String("relay.code", allyCode),
		attribute.Int("speaker.count", len(setup.joined)),
	)
	// Direct passthrough: single source (OneCaller) — skip entire mixer pipeline.
	// Raw Opus bytes flow directly from chIn to all speaker chOuts and relay session.
	if mode.IsDirectPassthrough() {
		session := &guild.Session{
			GuildID:  guildID,
			Cancel:   cancelFunc,
			Cleanup:  setup.speakerCleanup,
			AllyCode: allyCode,
			Speakers: setup.speakers,
			// ChannelMixers intentionally nil: UpdateMixerPause guards for nil already.
		}
		if err := m.commitSession(session); err != nil {
			setup.speakerCleanup()
			ownerCleanup()
			ov.Leave(ctx, guildID)
			m.sessions.RemoveHost(guildID)
			endSpanErr(span, err)
			return "", err
		}
		m.metrics.Session.SessionStarted(ctx, guildID, len(setup.joined))
		startFanoutDirect(ctx, chIn, setup.outs, allySession, guildID, &m.metrics.Session)
		startDirectSessionCleanup(ctx, ownerCleanup, guildID, &m.metrics.Session)
		slog.InfoContext(ctx, "voice raid started (direct passthrough)",
			slog.String("guildID", guildID.String()),
			slog.String("mode", string(mode)),
			slog.String("code", allyCode),
			slog.Int("activeSpeakers", len(setup.joined)),
		)
		return allyCode, nil
	}
	sources := buildSources(ctx, m.ownerBotID, ov.ChannelID(), chIn, setup.joined)
	destinations := buildDestinations(setup.joined)
	// Add the owner's channel as a playback destination so its mix-minus mixer
	// output reaches the owner bot's voice provider.
	if chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: ov.ChannelID(),
			outs:      []chan<- []byte{chOwnerOut},
		})
	}
	relayMixer, err := opus.NewMixer(&m.metrics.Mixer)
	if err != nil {
		setup.speakerCleanup()
		ownerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("create relay mixer: %w", err)
	}
	// Star topology: only the hub mixer (owner channel) does real mixing.
	// Speaker channels have exactly one audio source (owner) so no mixer is needed —
	// raw Opus bytes go directly to their chOuts via runFanoutOwnerStar.
	if mode.IsStarTopology() {
		hubMixer, err := opus.NewMixer(&m.metrics.Mixer)
		if err != nil {
			setup.speakerCleanup()
			ownerCleanup()
			endSpanErr(span, err)
			return "", fmt.Errorf("create hub mixer: %w", err)
		}
		channelMixers := map[snowflake.ID]*opus.Mixer{ov.ChannelID(): hubMixer}
		mixerPausers := map[snowflake.ID]guild.MixerPauser{ov.ChannelID(): hubMixer}
		session := &guild.Session{
			GuildID:       guildID,
			Cancel:        cancelFunc,
			Cleanup:       setup.speakerCleanup,
			AllyCode:      allyCode,
			Speakers:      setup.speakers,
			ChannelMixers: mixerPausers,
		}
		if err := m.commitSession(session); err != nil {
			setup.speakerCleanup()
			ownerCleanup()
			ov.Leave(ctx, guildID)
			m.sessions.RemoveHost(guildID)
			endSpanErr(span, err)
			return "", err
		}
		m.syncMixerPauseState(guildID, session)
		m.metrics.Session.SessionStarted(ctx, guildID, len(setup.joined))
		// Partition destinations into the owner hub and direct speaker outputs in one pass.
		var ownerDests []*destChannel
		var directSpeakerOuts []chan<- []byte
		for _, dest := range destinations {
			if dest.channelID == ov.ChannelID() {
				ownerDests = append(ownerDests, dest)
			} else {
				directSpeakerOuts = append(directSpeakerOuts, dest.outs...)
			}
		}
		wireFanoutOneManyDirect(ctx, guildID, sources, ov.ChannelID(), directSpeakerOuts, channelMixers, relayMixer, &m.metrics.Session)
		// Guest relay enters only at the hub mixer.
		if mode.AllowGuestCapture() {
			registerRelayInputs(ctx, guildID, allySession, ownerDests, channelMixers, &m.metrics.Session)
		}
		slog.InfoContext(ctx, "voice raid started (star direct)",
			slog.String("guildID", guildID.String()),
			slog.String("mode", string(mode)),
			slog.String("code", allyCode),
			slog.Int("activeSpeakers", len(setup.joined)),
		)
		// Start only the hub mixer; speaker chOuts are closed by runFanoutOwnerStar on exit.
		startChannelMixers(ctx, ownerDests, channelMixers)
		startRelayBroadcast(ctx, relayMixer, allySession, ownerCleanup, guildID, &m.metrics.Session)
		return allyCode, nil
	}
	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer(&m.metrics.Mixer)
		if err != nil {
			setup.speakerCleanup()
			ownerCleanup()
			endSpanErr(span, err)
			return "", fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}
	mixerPausers := make(map[snowflake.ID]guild.MixerPauser, len(channelMixers))
	for chID, mx := range channelMixers {
		mixerPausers[chID] = mx
	}
	session := &guild.Session{
		GuildID:       guildID,
		Cancel:        cancelFunc,
		Cleanup:       setup.speakerCleanup,
		AllyCode:      allyCode,
		Speakers:      setup.speakers,
		ChannelMixers: mixerPausers,
	}
	if err := m.commitSession(session); err != nil {
		setup.speakerCleanup()
		ownerCleanup()
		ov.Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
		endSpanErr(span, err)
		return "", err
	}
	// Pause mixers for channels that currently have no non-bot listeners.
	m.syncMixerPauseState(guildID, session)
	m.metrics.Session.SessionStarted(ctx, guildID, len(setup.joined))
	wireFanout(ctx, guildID, sources, destinations, channelMixers, relayMixer, &m.metrics.Session)
	// When the host allows guest capture, register host channel mixers as relay
	// receivers so BroadcastFromGuild packets from AllyCaller guests reach host speakers.
	if mode.AllowGuestCapture() {
		registerRelayInputs(ctx, guildID, allySession, destinations, channelMixers, &m.metrics.Session)
	}
	slog.InfoContext(ctx, "voice raid started",
		slog.String("guildID", guildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", allyCode),
		slog.Int("activeSpeakers", len(setup.joined)),
	)
	startChannelMixers(ctx, destinations, channelMixers)
	startRelayBroadcast(ctx, relayMixer, allySession, ownerCleanup, guildID, &m.metrics.Session)
	return allyCode, nil
}
