package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	hraban "github.com/hraban/opus"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// voiceLeaveTimeout is the maximum time to wait for a voice Leave call.
// Using context.Background() without a deadline risks hanging forever if Discord
// is unresponsive during session teardown.
const voiceLeaveTimeout = 5 * time.Second

// relayInputID is the synthetic source ID used when adding a guest relay feed
// as an input to a host-side ChannelMixer. Discord snowflakes are epoch-based
// (minimum value ~4 billion) so 1 never collides with a real user/bot ID.
const relayInputID snowflake.ID = 1

// sourceEntry is one audio capture channel feeding the relay mixer graph.
type sourceEntry struct {
	id        snowflake.ID
	channelID snowflake.ID
	ch        <-chan []byte
}

// destChannel groups all speaker output channels that share the same voice channel.
type destChannel struct {
	channelID snowflake.ID
	outs      []chan<- []byte
}

// speakerResult holds the outcome of a single successfully joined speaker.
type speakerResult struct {
	speaker   guild.Speaker
	chOut     chan<- []byte
	chCapture <-chan []byte // nil when withCapture is false
	gv        pool.GuildVoice
	cleanup   func() // closes provider/receiver; caller must invoke on teardown
}

// raidSetup captures the common setup result for both host and guest flows.
type raidSetup struct {
	joined         []speakerResult
	speakers       []guild.Speaker
	speakerCleanup func()
	outs           []chan<- []byte
}

// setupSpeakers snapshots and joins all enabled, bound speakers for a guild.
// Returns an error if the guild has no status, already has an active session,
// or no speakers could join.
func (m *Service) setupSpeakers(ctx context.Context, guildID snowflake.ID, mode guild.RaidMode, allowUser func(snowflake.ID) bool) (*raidSetup, error) {
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return nil, err
	}

	joined := m.joinSpeakers(ctx, guildID, speakers, mode.WithCapture(), allowUser)
	if len(joined) == 0 {
		return nil, fmt.Errorf("no speakers joined: verify speaker channels are bound and bots are online in this guild")
	}

	outs := make([]chan<- []byte, 0, len(joined))
	joinedSpeakers := make([]guild.Speaker, 0, len(joined))
	for _, r := range joined {
		outs = append(outs, r.chOut)
		joinedSpeakers = append(joinedSpeakers, r.speaker)
	}

	return &raidSetup{
		joined:         joined,
		speakers:       joinedSpeakers,
		speakerCleanup: buildSpeakerCleanup(guildID, joined),
		outs:           outs,
	}, nil
}

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

		guestChannelMixers = make(map[snowflake.ID]*opus.Mixer, len(destinations))
		for _, dest := range destinations {
			mx, err := opus.NewMixer()
			if err != nil {
				setup.speakerCleanup()
				guestCleanupOwner()
				m.sessions.RemoveGuest(guestGuildID)
				endSpanErr(span, err)
				return guestMode, fmt.Errorf("guest: create channel mixer: %w", err)
			}
			guestChannelMixers[dest.channelID] = mx
		}

		guestRelayMixer, err = opus.NewMixer()
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
		sources := buildGuestSources(setup.joined)
		// Include the owner bot's capture channel as a source so users speaking
		// in the owner's channel are mixed and relayed — the missing half of the
		// guest AllyCaller flow that speakers already provide for their channels.
		if ownerChIn != nil {
			sources = append(sources, sourceEntry{m.ownerBotID, ownerVoice.ChannelID(), ownerChIn})
		}
		if guestMode.IsStarTopology() {
			// Guest star: all sources → relay only; channel mixers fed by registerRelayInputs.
			wireFanoutOneMany(ctx, sources, destinations, guestChannelMixers, guestRelayMixer, 0)
		} else {
			wireFanout(ctx, sources, destinations, guestChannelMixers, guestRelayMixer)
		}
		toClose = registerRelayInputs(guestGuildID, allySession, destinations, guestChannelMixers)
		startChannelMixers(ctx, destinations, guestChannelMixers)
		startGuestRelayBroadcast(ctx, guestRelayMixer, allySession, guestGuildID)
	} else {
		outs := make([]chan<- []byte, len(setup.outs))
		copy(outs, setup.outs)
		if ownerChOut != nil {
			outs = append(outs, ownerChOut)
		}
		allySession.AddGuild(guestGuildID, outs)
		toClose = outs
	}

	telemetry.SessionsActive.Add(ctx, 1)
	telemetry.SessionStart.Add(ctx, 1)

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
			telemetry.SessionsActive.Add(ctx, -1)
			telemetry.SessionStop.Add(ctx, 1)
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
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return "", err
	}

	ov := m.ownerVoice(guildID)
	conn, err := ov.Join(ctx, guildID)
	if err != nil {
		setup.speakerCleanup()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return "", fmt.Errorf("failed to join owner channel: %w", err)
	}
	if conn == nil {
		setup.speakerCleanup()
		err = fmt.Errorf("no voice connection to owner channel")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
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

	sources := buildSources(m.ownerBotID, ov.ChannelID(), chIn, setup.joined)
	destinations := buildDestinations(setup.joined)

	// Add the owner's channel as a playback destination so its mix-minus mixer
	// output reaches the owner bot's voice provider.
	if chOwnerOut != nil {
		destinations = append(destinations, &destChannel{
			channelID: ov.ChannelID(),
			outs:      []chan<- []byte{chOwnerOut},
		})
	}

	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer()
		if err != nil {
			setup.speakerCleanup()
			ownerCleanup()
			endSpanErr(span, err)
			return "", fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}

	relayMixer, err := opus.NewMixer()
	if err != nil {
		setup.speakerCleanup()
		ownerCleanup()
		endSpanErr(span, err)
		return "", fmt.Errorf("create relay mixer: %w", err)
	}

	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)

	span.SetAttributes(
		attribute.String("relay.code", allyCode),
		attribute.Int("speaker.count", len(setup.joined)),
	)

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

	telemetry.SessionsActive.Add(ctx, 1)
	telemetry.SessionStart.Add(ctx, 1)

	if mode.IsStarTopology() {
		wireFanoutOneMany(ctx, sources, destinations, channelMixers, relayMixer, ov.ChannelID())
	} else {
		wireFanout(ctx, sources, destinations, channelMixers, relayMixer)
	}

	// When the host allows guest capture, register host channel mixers as relay
	// receivers so BroadcastFromGuild packets from AllyCaller guests reach host speakers.
	// In star topology, guest relay only reaches the owner's channel mixer (hub).
	if mode.AllowGuestCapture() {
		if mode.IsStarTopology() {
			ownerDests := make([]*destChannel, 0, 1)
			for _, d := range destinations {
				if d.channelID == ov.ChannelID() {
					ownerDests = append(ownerDests, d)
					break
				}
			}
			registerRelayInputs(guildID, allySession, ownerDests, channelMixers)
		} else {
			registerRelayInputs(guildID, allySession, destinations, channelMixers)
		}
	}

	slog.InfoContext(ctx, "voice raid started",
		slog.String("guildID", guildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", allyCode),
		slog.Int("activeSpeakers", len(setup.joined)),
	)

	startChannelMixers(ctx, destinations, channelMixers)
	startRelayBroadcast(ctx, relayMixer, allySession, ownerCleanup, guildID)

	return allyCode, nil
}

// joinSpeakers joins all enabled, bound speakers in parallel.
// When withCapture is true each speaker also captures incoming frames, filtered by allowUser.
func (m *Service) joinSpeakers(ctx context.Context, guildID snowflake.ID, speakers []guild.Speaker, withCapture bool, allowUser func(snowflake.ID) bool) []speakerResult {
	var candidates []guild.Speaker
	for _, sp := range speakers {
		if sp.Enabled {
			if _, ok := m.store.GetBoundChannel(guildID, sp.ID); ok {
				candidates = append(candidates, sp)
			}
		}
	}

	resultCh := make(chan speakerResult, len(candidates))
	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for _, sp := range candidates {
		go func(sp guild.Speaker) {
			defer wg.Done()
			gv, ok := m.speakerVoice(guildID, sp.ID)
			if !ok {
				slog.WarnContext(ctx, "speaker not in pool", slog.String("speakerID", sp.ID.String()))
				return
			}
			conn, err := gv.Join(ctx, guildID)
			if err != nil {
				slog.WarnContext(ctx, "speaker failed to join channel", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				return
			}
			if withCapture {
				m.prefetchChannelMembers(ctx, conn, sp.ID, guildID)
			}
			chOut := make(chan []byte, audioChanBuf)
			chCapture, cleanup, err := m.consumeSpeaker(ctx, sp.ID, conn, chOut, withCapture, allowUser)
			if err != nil {
				slog.ErrorContext(ctx, "failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				gv.Leave(ctx, guildID)
				return
			}
			resultCh <- speakerResult{sp, chOut, chCapture, gv, cleanup}
		}(sp)
	}
	wg.Wait()
	close(resultCh)

	results := make([]speakerResult, 0, len(candidates))
	for r := range resultCh {
		results = append(results, r)
	}
	return results
}

// commitSession stores session under write lock, re-checking for conflicts.
func (m *Service) commitSession(session *guild.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statuses[session.GuildID]
	if st == nil {
		return fmt.Errorf("guild status disappeared before session could be stored")
	}
	if st.HasActiveSession() {
		return fmt.Errorf("a voice raid is already active in this server")
	}
	st.Session = session
	return nil
}

// consumeSpeaker sets up audio provider and receiver for a speaker's voice connection.
// chOut is the provider channel (frames to play). When withCapture is true the
// returned channel receives frames captured from the speaker's channel, filtered
// by allowUser (shared filter built once at session start).
// The caller is responsible for calling the returned cleanup function.
func (m *Service) consumeSpeaker(ctx context.Context, speakerID snowflake.ID, conn voice.Conn, chOut <-chan []byte, withCapture bool, allowUser func(snowflake.ID) bool) (chan []byte, func(), error) {

	session := NewVoiceConnSetup(speakerID)
	if m.test.IsTestBot(speakerID) {
		session.WithFileProvider(m.test.FileDCA)
	} else {
		session.WithVoiceProvider()
	}

	if withCapture {
		session.WithVoiceReceiver(allowUser)
	}

	capture, cleanup, err := session.Apply(ctx, conn, chOut)
	if err != nil {
		return nil, nil, err
	}

	return capture, cleanup, nil
}

// iterDeduplicatedCaptures calls fn for the first capture channel per voice
// channel across joined. Any subsequent capture from the same channel is drained
// in a background goroutine to prevent the VoiceReceiver from blocking.
func iterDeduplicatedCaptures(joined []speakerResult, fn func(speakerResult)) {
	seen := map[snowflake.ID]bool{}
	for _, r := range joined {
		if r.chCapture == nil {
			continue
		}
		if seen[r.gv.ChannelID()] {
			go func(ch <-chan []byte) {
				for range ch {
				}
			}(r.chCapture)
			continue
		}
		seen[r.gv.ChannelID()] = true
		fn(r)
	}
}

// buildSources returns a deduplicated list of audio sources (one capture channel per voice
// channel). When two speaker bots share a channel the second capture is drained and discarded.
func buildSources(ownerUserID, ownerChannelID snowflake.ID, chIn chan []byte, joined []speakerResult) []sourceEntry {
	sources := []sourceEntry{{ownerUserID, ownerChannelID, chIn}}
	iterDeduplicatedCaptures(joined, func(r speakerResult) {
		sources = append(sources, sourceEntry{r.speaker.ID, r.gv.ChannelID(), r.chCapture})
	})
	return sources
}

// buildDestinations groups each speaker's output channel by its destination voice channel.
func buildDestinations(joined []speakerResult) []*destChannel {
	destMap := map[snowflake.ID]*destChannel{}
	for _, r := range joined {
		d, ok := destMap[r.gv.ChannelID()]
		if !ok {
			d = &destChannel{channelID: r.gv.ChannelID()}
			destMap[r.gv.ChannelID()] = d
		}
		d.outs = append(d.outs, r.chOut)
	}
	dests := make([]*destChannel, 0, len(destMap))
	for _, d := range destMap {
		dests = append(dests, d)
	}
	return dests
}

// mixerRef pairs a mixer with the source ID registered in it, so the fanout
// goroutine can call RemoveInput when the source channel is exhausted.
type mixerRef struct {
	mx *opus.Mixer
	id snowflake.ID
}

// wireFanout starts a goroutine per source that decodes each incoming Opus packet
// exactly once and distributes the resulting PCM to all relevant mixer inputs.
// Decoding once (vs once per mixer) cuts decode operations from sources×mixers to
// sources. The relay mixer receives every source; per-channel mixers skip the source
// from their own channel (mix-minus).
// Each goroutine calls RemoveInput on every mixer it registered when it exits,
// so stale entries are not retained for the lifetime of the session.
func wireFanout(ctx context.Context, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer) {
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		relayCh := make(chan opus.Frame, audioChanBuf)
		if err := relayMixer.AddInput(src.id, relayCh); err != nil {
			slog.WarnContext(ctx, "relay mixer: failed to add input", slog.Any("err", err))
		} else {
			fanTargets = append(fanTargets, relayCh)
			removals = append(removals, mixerRef{relayMixer, src.id})
		}

		for _, dest := range dests {
			if dest.channelID == src.channelID {
				continue // mix-minus: don't relay audio back to its origin channel
			}
			mixCh := make(chan opus.Frame, audioChanBuf)
			if err := chanMixers[dest.channelID].AddInput(src.id, mixCh); err != nil {
				slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
			} else {
				fanTargets = append(fanTargets, mixCh)
				removals = append(removals, mixerRef{chanMixers[dest.channelID], src.id})
			}
		}

		go func(in <-chan []byte, targets []chan opus.Frame, removals []mixerRef) {
			defer func() {
				for _, r := range removals {
					r.mx.RemoveInput(r.id)
				}
			}()
			dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
			if err != nil {
				slog.ErrorContext(ctx, "wireFanout: failed to create decoder", slog.Any("err", err))
				return
			}
			scratch := make([]int16, opus.MixerPCMBuf)
			for {
				select {
				case <-ctx.Done():
					return
				case pkt, ok := <-in:
					if !ok {
						return
					}
					if len(pkt) == 0 {
						continue // DTX silence — nothing to decode or distribute
					}
					// Decode once; distribute a Frame carrying both PCM and the original
					// Opus bytes. The mixer uses PCM when mixing multiple sources and
					// forwards Opus directly when only one source is active (point 8).
					//
					// READ-ONLY CONTRACT: pkt is the slice received from the Discord
					// VoiceReceiver and is shared across every Frame sent to targets.
					// No consumer may mutate pkt. The mixer copies it before forwarding
					// to its output channel (see Mixer.tick single-source path), so
					// downstream consumers always get their own slice.
					n, err := dec.Decode(pkt, scratch)
					if err != nil {
						slog.Debug("wireFanout: decode failed", slog.Any("err", err))
						continue
					}
					pcm := make([]int16, n*opus.MixerChannels)
					copy(pcm, scratch[:n*opus.MixerChannels])
					frame := opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: time.Now()}
					for _, t := range targets {
						select {
						case t <- frame:
						default:
						}
					}
				}
			}
		}(src.ch, fanTargets, removals)
	}
}

// wireFanoutOneMany implements a star-topology fanout where the owner channel is
// the central hub. The owner source fans out to all destination channel mixers
// (mix-minus), but speaker sources fan out ONLY to the owner's channel mixer —
// speakers cannot hear each other.
//
// ownerChannelID identifies the hub channel. When 0 (guest star mode), ALL
// sources go to the relay mixer only — no local channel-to-channel routing.
// The guest's channel mixers receive audio solely via registerRelayInputs
// (the host's relay), ensuring guest speakers hear only the host owner.
func wireFanoutOneMany(ctx context.Context, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer, ownerChannelID snowflake.ID) {
	for _, src := range sources {
		var fanTargets []chan opus.Frame
		var removals []mixerRef

		// All sources always feed the relay mixer.
		relayCh := make(chan opus.Frame, audioChanBuf)
		if err := relayMixer.AddInput(src.id, relayCh); err != nil {
			slog.WarnContext(ctx, "relay mixer: failed to add input", slog.Any("err", err))
		} else {
			fanTargets = append(fanTargets, relayCh)
			removals = append(removals, mixerRef{relayMixer, src.id})
		}

		if ownerChannelID != 0 {
			if src.channelID == ownerChannelID {
				// Owner source → all channel mixers except its own (standard mix-minus).
				for _, dest := range dests {
					if dest.channelID == src.channelID {
						continue
					}
					mixCh := make(chan opus.Frame, audioChanBuf)
					if err := chanMixers[dest.channelID].AddInput(src.id, mixCh); err != nil {
						slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
					} else {
						fanTargets = append(fanTargets, mixCh)
						removals = append(removals, mixerRef{chanMixers[dest.channelID], src.id})
					}
				}
			} else {
				// Speaker source → owner channel mixer ONLY (star spoke → hub).
				if ownerMixer, ok := chanMixers[ownerChannelID]; ok {
					mixCh := make(chan opus.Frame, audioChanBuf)
					if err := ownerMixer.AddInput(src.id, mixCh); err != nil {
						slog.WarnContext(ctx, "channel mixer: failed to add input", slog.Any("err", err))
					} else {
						fanTargets = append(fanTargets, mixCh)
						removals = append(removals, mixerRef{ownerMixer, src.id})
					}
				}
			}
		}
		// When ownerChannelID == 0 (guest star), sources go to relay only.

		go func(in <-chan []byte, targets []chan opus.Frame, removals []mixerRef) {
			defer func() {
				for _, r := range removals {
					r.mx.RemoveInput(r.id)
				}
			}()
			dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
			if err != nil {
				slog.ErrorContext(ctx, "wireFanoutOneMany: failed to create decoder", slog.Any("err", err))
				return
			}
			scratch := make([]int16, opus.MixerPCMBuf)
			for {
				select {
				case <-ctx.Done():
					return
				case pkt, ok := <-in:
					if !ok {
						return
					}
					if len(pkt) == 0 {
						continue
					}
					n, err := dec.Decode(pkt, scratch)
					if err != nil {
						slog.Debug("wireFanoutOneMany: decode failed", slog.Any("err", err))
						continue
					}
					pcm := make([]int16, n*opus.MixerChannels)
					copy(pcm, scratch[:n*opus.MixerChannels])
					frame := opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: time.Now()}
					for _, t := range targets {
						select {
						case t <- frame:
						default:
						}
					}
				}
			}
		}(src.ch, fanTargets, removals)
	}
}

// startChannelMixers runs each per-channel mixer and forwards its output to all
// speaker output channels in that destination, closing them when the mixer stops.
func startChannelMixers(ctx context.Context, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) {
	for _, dest := range dests {
		mx := chanMixers[dest.channelID]
		destOuts := dest.outs
		go mx.Run(ctx)
		go func(mx *opus.Mixer, destOuts []chan<- []byte) {
			for pkt := range mx.Output() {
				for _, out := range destOuts {
					select {
					case out <- pkt:
					default:
					}
				}
			}
			for _, out := range destOuts {
				close(out)
			}
		}(mx, destOuts)
	}
}

// startRelayBroadcast runs the relay mixer and broadcasts its output to all guest guilds.
// Calls ownerCleanup only after the mixer has fully stopped and its output channel is closed,
// ensuring no in-flight frames are lost and cleanup is ordered after the last broadcast.
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func(), guildID snowflake.ID) {
	go func() {
		defer func() {
			ownerCleanup()
			telemetry.SessionsActive.Add(ctx, -1)
			telemetry.SessionStop.Add(ctx, 1)
			trace.SpanFromContext(ctx).End()
			slog.InfoContext(ctx, "voice raid ended", slog.String("guildID", guildID.String()))
		}()
		go relayMixer.Run(ctx)
		// Range blocks until the mixer closes its output channel (on ctx cancel),
		// guaranteeing all queued frames are broadcast before cleanup runs.
		for pkt := range relayMixer.Output() {
			relaySession.BroadcastFromGuild(guildID, pkt)
		}
	}()
}

// registerRelayInputs wires a guild as a relay receiver in the ally session.
// For each destination channel it creates:
//   - an Opus input channel (chan []byte) registered with the ally session, and
//   - a PCM bridge goroutine that decodes once and forwards []int16 to the channel mixer.
//
// The ally session keeps sending Opus packets; the bridge decodes each exactly once
// before the mixer accumulates it. Returns the Opus input channels so the caller
// can close them on teardown (closing triggers bridge goroutine exit).
func registerRelayInputs(guildID snowflake.ID, session *ally.Session, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer) []chan<- []byte {
	relayIns := make([]chan<- []byte, 0, len(dests))
	for _, dest := range dests {
		relayOpusIn := make(chan []byte, audioChanBuf)
		relayFrameOut := make(chan opus.Frame, audioChanBuf)
		if err := chanMixers[dest.channelID].AddInput(relayInputID, relayFrameOut); err != nil {
			slog.Warn("relay: failed to add input to channel mixer",
				slog.String("channelID", dest.channelID.String()),
				slog.Any("err", err))
			continue
		}
		// Bridge: decode relay Opus packets once and forward Frame to the mixer.
		// Both PCM and original Opus bytes are included so the mixer can apply
		// the single-source passthrough optimisation (point 8).
		// Exits when relayOpusIn is closed (session teardown closes it via toClose).
		go func(in <-chan []byte, out chan<- opus.Frame) {
			defer close(out)
			dec, err := hraban.NewDecoder(opus.MixerSampleRate, opus.MixerChannels)
			if err != nil {
				slog.Error("relay bridge: failed to create decoder", slog.Any("err", err))
				return
			}
			scratch := make([]int16, opus.MixerPCMBuf)
			for pkt := range in {
				// Drain to latest relay packet to avoid accumulating latency
				// across the inter-guild relay hop.
			drainRelay:
				for {
					select {
					case newer, ok := <-in:
						if !ok {
							break drainRelay
						}
						pkt = newer
					default:
						break drainRelay
					}
				}
				if len(pkt) == 0 {
					continue
				}
				n, err := dec.Decode(pkt, scratch)
				if err != nil {
					slog.Debug("relay bridge: decode failed", slog.Any("err", err))
					continue
				}
				pcm := make([]int16, n*opus.MixerChannels)
				copy(pcm, scratch[:n*opus.MixerChannels])
				select {
				case out <- opus.Frame{PCM: pcm, Opus: pkt, CreatedAt: time.Now()}:
				default:
				}
			}
		}(relayOpusIn, relayFrameOut)
		relayIns = append(relayIns, relayOpusIn)
	}
	if len(relayIns) > 0 {
		session.AddGuild(guildID, relayIns)
	}
	return relayIns
}

// buildGuestSources returns deduplicated capture sources from speaker joins.
// Unlike the host's buildSources, the guest owner bot is provider-only so
// it contributes no capture channel.
func buildGuestSources(joined []speakerResult) []sourceEntry {
	var sources []sourceEntry
	iterDeduplicatedCaptures(joined, func(r speakerResult) {
		sources = append(sources, sourceEntry{r.speaker.ID, r.gv.ChannelID(), r.chCapture})
	})
	return sources
}

// startGuestRelayBroadcast runs the guest relay mixer and broadcasts its output
// to all OTHER guilds via BroadcastFromGuild, excluding the guest itself.
func startGuestRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, session *ally.Session, guestGuildID snowflake.ID) {
	go relayMixer.Run(ctx)
	go func() {
		for pkt := range relayMixer.Output() {
			session.BroadcastFromGuild(guestGuildID, pkt)
		}
	}()
}

// buildSpeakerCleanup returns a function that closes every speaker's
// provider/receiver and leaves its voice channel, exactly once.
func buildSpeakerCleanup(guildID snowflake.ID, joined []speakerResult) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), voiceLeaveTimeout)
			defer cancel()
			for _, r := range joined {
				if r.cleanup != nil {
					r.cleanup()
				}
				r.gv.Leave(ctx, guildID)
			}
		})
	}
}

// snapshotSpeakers returns a deep copy of the guild's speakers as a slice.
// Returns an error if the guild has no status or already has an active session.
func (m *Service) snapshotSpeakers(guildID snowflake.ID) ([]guild.Speaker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	if st == nil {
		return nil, fmt.Errorf("no guild status found — seed the guild first")
	}
	if st.Session != nil {
		return nil, fmt.Errorf("a voice raid is already active in this server")
	}
	speakers := make([]guild.Speaker, 0, len(st.Speakers))
	for _, v := range st.Speakers {
		speakers = append(speakers, *v)
	}
	return speakers, nil
}

// endSpanErr records an error on a span and ends it.
func endSpanErr(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.End()
}
