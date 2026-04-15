package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

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
		return nil, fmt.Errorf("no speakers could join their voice channels")
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
	allySession, err := m.sessions.Join(code, guestGuildID)
	if err != nil {
		return guestMode, err
	}

	guestMode = guild.RaidModeAllyListener
	// Temporarily set guild.RaidModeAllyListener for all guests until fix bug with many to many relay and capture role sync.
	//// Downgrade to listen-only when the host doesn't allow guest capture.
	//if guestMode.WithCapture() && !allySession.HostMode.AllowGuestCapture() {
	//	guestMode = guild.RaidModeAllyListener
	//}

	allowUser := m.buildAllowUserFilter(guestGuildID)
	setup, err := m.setupSpeakers(ctx, guestGuildID, guestMode, allowUser)
	if err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		return guestMode, err
	}

	// In AllyCaller mode relay guest captures to all OTHER guilds (never back to
	// the originating guild — that would echo audio to the users who spoke).
	if guestMode.WithCapture() {
		iterDeduplicatedCaptures(setup.joined, func(r speakerResult) {
			go func(ch <-chan []byte) {
				for pkt := range ch {
					allySession.BroadcastFromGuild(guestGuildID, pkt)
				}
			}(r.chCapture)
		})
	}

	// Join the owner bot as a relayer into its bound channel.
	ownerVoice := m.ownerVoice(guestGuildID)
	var ownerCleanup func()
	if conn, err := ownerVoice.Join(ctx, guestGuildID); err != nil {
		slog.Warn("guest: failed to join owner channel", slog.Any("err", err))
	} else if conn != nil {
		chOut := make(chan []byte, audioChanBuf)
		_, cleanup, err := NewVoiceConnSetup(m.ownerBotID).WithVoiceProvider().Apply(ctx, conn, chOut)
		if err != nil {
			slog.Warn("guest: failed to setup owner relay", slog.Any("err", err))
		} else {
			ownerCleanup = cleanup
			setup.outs = append(setup.outs, chOut)
		}
	}

	allySession.AddGuild(guestGuildID, setup.outs)

	session := &guild.Session{
		GuildID:  guestGuildID,
		Cancel:   cancelFunc,
		Cleanup:  setup.speakerCleanup,
		AllyCode: code,
		IsGuest:  true,
		Speakers: setup.speakers,
	}
	if err := m.commitSession(session); err != nil {
		setup.speakerCleanup()
		if ownerCleanup != nil {
			ownerCleanup()
			ownerVoice.Leave(ctx, guestGuildID)
		}
		m.sessions.RemoveGuest(guestGuildID)
		return guestMode, fmt.Errorf("failed to commit session: %w", err)
	}

	slog.Info("guest joined relay session",
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
			if ownerCleanup != nil {
				ownerCleanup()
				ownerVoice.Leave(context.Background(), guestGuildID)
			}
			// Remove from relay BEFORE closing channels to prevent send-on-closed-channel.
			allySession.RemoveGuild(guestGuildID)
			for _, out := range setup.outs {
				close(out)
			}
			m.sessions.RemoveGuest(guestGuildID)
			m.mu.Lock()
			if st := m.statuses[guestGuildID]; st != nil {
				st.Session = nil
			}
			m.mu.Unlock()
			slog.Info("guest session ended", slog.String("guildID", guestGuildID.String()))
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

	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
// mode controls which channels capture audio; guests can always join via the relay code.
// Returns the relay session code.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode guild.RaidMode) (ally.Code, error) {
	allowUser := m.buildAllowUserFilter(guildID)
	setup, err := m.setupSpeakers(ctx, guildID, mode, allowUser)
	if err != nil {
		return "", err
	}

	ov := m.ownerVoice(guildID)
	conn, err := ov.Join(ctx, guildID)
	if err != nil {
		setup.speakerCleanup()
		return "", fmt.Errorf("failed to join owner channel: %w", err)
	}
	if conn == nil {
		setup.speakerCleanup()
		return "", fmt.Errorf("no voice connection to owner channel")
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
			return "", fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}

	relayMixer, err := opus.NewMixer()
	if err != nil {
		setup.speakerCleanup()
		ownerCleanup()
		return "", fmt.Errorf("create relay mixer: %w", err)
	}

	allyCode := m.store.GetOrCreateAllyCode(guildID)
	allySession := m.sessions.Create(allyCode, guildID, mode)

	wireFanout(ctx, sources, destinations, channelMixers, relayMixer)

	session := &guild.Session{
		GuildID:  guildID,
		Cancel:   cancelFunc,
		Cleanup:  setup.speakerCleanup,
		AllyCode: allyCode,
		Speakers: setup.speakers,
	}
	if err := m.commitSession(session); err != nil {
		setup.speakerCleanup()
		ownerCleanup()
		ov.Leave(ctx, guildID)
		m.sessions.RemoveHost(guildID)
		return "", err
	}

	slog.Info("voice raid started",
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
				slog.Warn("speaker not in pool", slog.String("speakerID", sp.ID.String()))
				return
			}
			conn, err := gv.Join(ctx, guildID)
			if err != nil {
				slog.Warn("speaker failed to join channel", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				return
			}
			if withCapture {
				m.prefetchChannelMembers(ctx, conn, sp.ID, guildID)
			}
			chOut := make(chan []byte, audioChanBuf)
			chCapture, cleanup, err := m.consumeSpeaker(ctx, sp.ID, conn, chOut, withCapture, allowUser)
			if err != nil {
				slog.Error("failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
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

// wireFanout starts a goroutine per source that copies each incoming packet to all
// relevant mixer inputs. The relay mixer receives every source; per-channel mixers
// skip the source from their own channel (mix-minus).
func wireFanout(ctx context.Context, sources []sourceEntry, dests []*destChannel, chanMixers map[snowflake.ID]*opus.Mixer, relayMixer *opus.Mixer) {
	for _, src := range sources {
		var fanTargets []chan []byte

		relayCh := make(chan []byte, audioChanBuf)
		if err := relayMixer.AddInput(src.id, relayCh); err != nil {
			slog.Warn("relay mixer: failed to add input", slog.Any("err", err))
		} else {
			fanTargets = append(fanTargets, relayCh)
		}

		for _, dest := range dests {
			if dest.channelID == src.channelID {
				continue // mix-minus: don't relay audio back to its origin channel
			}
			mixCh := make(chan []byte, audioChanBuf)
			if err := chanMixers[dest.channelID].AddInput(src.id, mixCh); err != nil {
				slog.Warn("channel mixer: failed to add input", slog.Any("err", err))
			} else {
				fanTargets = append(fanTargets, mixCh)
			}
		}

		go func(in <-chan []byte, targets []chan []byte) {
			for {
				select {
				case <-ctx.Done():
					return
				case pkt, ok := <-in:
					if !ok {
						return
					}
					for _, t := range targets {
						select {
						case t <- pkt:
						default:
						}
					}
				}
			}
		}(src.ch, fanTargets)
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
// Calls ownerCleanup and logs when the context is cancelled.
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *ally.Session, ownerCleanup func(), guildID snowflake.ID) {
	go relayMixer.Run(ctx)
	go func() {
		defer func() {
			ownerCleanup()
			slog.Info("voice raid ended", slog.String("guildID", guildID.String()))
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-relayMixer.Output():
				if !ok {
					return
				}
				relaySession.Broadcast(pkt)
			}
		}
	}()
}

// buildSpeakerCleanup returns a function that closes every speaker's
// provider/receiver and leaves its voice channel, exactly once.
func buildSpeakerCleanup(guildID snowflake.ID, joined []speakerResult) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, r := range joined {
				if r.cleanup != nil {
					r.cleanup()
				}
				r.gv.Leave(context.Background(), guildID)
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
