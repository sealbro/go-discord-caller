package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/relay"
	"github.com/sealbro/go-discord-caller/internal/store"
)

type closer interface{ Close() }

// sourceEntry is one audio capture channel feeding the relay mixer graph.
type sourceEntry struct {
	id        snowflake.ID
	channelID snowflake.ID
	ch        chan []byte
}

// destChannel groups all speaker output channels that share the same voice channel.
type destChannel struct {
	channelID snowflake.ID
	outs      []chan []byte
}

// JoinSession connects this guild as a guest to an existing relay session.
// mode must be a guest mode: RaidModeGuestOne (listener only) or RaidModeAllyCaller
// (speakers also capture from their channels for local mixing).
// The session ends automatically when the host ends or ctx is cancelled.
func (m *Service) JoinSession(ctx context.Context, guestGuildID snowflake.ID, cancelFunc context.CancelFunc, mode domain.RaidMode, code relay.RelayCode) error {
	relaySession, err := m.sessions.Join(code, guestGuildID)
	if err != nil {
		return err
	}

	speakers, err := m.snapshotSpeakers(guestGuildID)
	if err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		return err
	}

	joined := m.joinSpeakers(ctx, guestGuildID, speakers, mode.WithCapture())

	// Collect per-speaker output channels; the owner's relay channel is appended below.
	outs := make([]chan []byte, 0, len(joined)+1)
	for _, r := range joined {
		outs = append(outs, r.chOut)
	}

	// Join the owner bot as a relayer into its bound channel.
	var ownerProvider *opus.VoiceProvider
	if err := m.JoinChannel(ctx, guestGuildID, m.ownerBotID); err != nil {
		slog.Warn("guest: failed to join owner channel", slog.Any("err", err))
	} else if conn := m.getOwnerClient().VoiceManager.GetConn(guestGuildID); conn != nil {
		chOut, provider, err := m.setupOwnerRelay(ctx, conn)
		if err != nil {
			slog.Warn("guest: failed to setup owner relay", slog.Any("err", err))
		} else {
			ownerProvider = provider
			outs = append(outs, chOut)
		}
	}

	relaySession.AddGuild(guestGuildID, outs)

	joinedSpeakers := make([]*domain.Speaker, len(joined))
	for i, r := range joined {
		joinedSpeakers[i] = r.speaker
	}
	session := &domain.VoiceSession{
		GuildID:   guestGuildID,
		Cancel:    cancelFunc,
		RelayCode: code,
		IsGuest:   true,
		Speakers:  joinedSpeakers,
	}
	if err := m.commitSession(guestGuildID, session); err != nil {
		m.sessions.RemoveGuest(guestGuildID)
		return err
	}

	slog.Info("guest joined relay session",
		slog.String("guildID", guestGuildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", code),
		slog.Int("activeSpeakers", len(joined)),
		slog.Bool("ownerRelaying", ownerProvider != nil),
	)

	go func() {
		defer func() {
			// Leave channels before closing audio channels so consumers are not
			// reading from a closed channel while still connected.
			for _, r := range joined {
				m.poolSvc.LeaveChannel(context.Background(), guestGuildID, r.speaker.ID)
			}
			if ownerProvider != nil {
				ownerProvider.Close()
				m.LeaveChannel(context.Background(), guestGuildID, m.ownerBotID)
			}
			for _, out := range outs {
				close(out)
			}
			relaySession.RemoveGuild(guestGuildID)
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
		case <-relaySession.Done():
			cancelFunc()
		}
	}()

	return nil
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
	ownerUserID := status.OwnerUserID
	status.Session = nil
	m.mu.Unlock()

	// Cancel first to stop the relay broadcast goroutine before closing channels.
	session.Cancel()
	for _, sp := range session.Speakers {
		m.poolSvc.LeaveChannel(ctx, guildID, sp.ID)
	}
	if !session.IsGuest {
		m.LeaveChannel(ctx, guildID, ownerUserID)
		m.sessions.RemoveHost(guildID)
	}

	slog.Info("voice raid stopped", slog.String("guildID", guildID.String()))
	return nil
}

// StartVoiceRaid makes all enabled, bound speakers join their voice channels.
// mode controls which channels capture audio; guests can always join via the relay code.
// Returns the relay session code.
func (m *Service) StartVoiceRaid(ctx context.Context, guildID snowflake.ID, cancelFunc context.CancelFunc, mode domain.RaidMode) (relay.RelayCode, error) {
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return "", err
	}

	if err := m.JoinChannel(ctx, guildID, m.ownerBotID); err != nil {
		return "", fmt.Errorf("failed to join owner channel: %w", err)
	}
	conn := m.getOwnerClient().VoiceManager.GetConn(guildID)
	if conn == nil {
		return "", fmt.Errorf("no voice connection to owner channel")
	}

	chIn, ownerReceiver, ownerProvider, err := m.setupOwnerCapture(ctx, conn, m.ownerBotID, guildID)
	if err != nil {
		return "", err
	}

	joined := m.joinSpeakers(ctx, guildID, speakers, mode.WithCapture())

	ownerChannelID, _ := m.store.GetBoundChannel(guildID, m.ownerBotID)
	sources := buildSources(m.ownerBotID, ownerChannelID, chIn, joined)
	destinations := buildDestinations(joined)

	channelMixers := make(map[snowflake.ID]*opus.Mixer, len(destinations))
	for _, dest := range destinations {
		mx, err := opus.NewMixer()
		if err != nil {
			return "", fmt.Errorf("create channel mixer: %w", err)
		}
		channelMixers[dest.channelID] = mx
	}

	relayMixer, err := opus.NewMixer()
	if err != nil {
		return "", fmt.Errorf("create relay mixer: %w", err)
	}
	relayCode := m.store.GetOrCreateRelayCode(guildID)
	relaySession := m.sessions.Create(relayCode, guildID)

	wireFanout(ctx, sources, destinations, channelMixers, relayMixer)

	joinedSpeakers := make([]*domain.Speaker, len(joined))
	for i, r := range joined {
		joinedSpeakers[i] = r.speaker
	}
	session := &domain.VoiceSession{
		GuildID:   guildID,
		Cancel:    cancelFunc,
		RelayCode: relayCode,
		Speakers:  joinedSpeakers,
	}
	if err := m.commitSession(guildID, session); err != nil {
		return "", err
	}

	slog.Info("voice raid started",
		slog.String("guildID", guildID.String()),
		slog.String("mode", string(mode)),
		slog.String("code", relayCode),
		slog.Int("activeSpeakers", len(joined)),
	)

	startChannelMixers(ctx, destinations, channelMixers)
	startRelayBroadcast(ctx, relayMixer, relaySession, ownerReceiver, ownerProvider, guildID)

	return relayCode, nil
}

// joinSpeakers joins all enabled, bound speakers in parallel.
// When withCapture is true each speaker also captures incoming frames.
func (m *Service) joinSpeakers(ctx context.Context, guildID snowflake.ID, speakers []*domain.Speaker, withCapture bool) []speakerResult {
	type candidate struct {
		speaker   *domain.Speaker
		channelID snowflake.ID
	}
	var candidates []candidate
	for _, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		if channelID, ok := m.store.GetBoundChannel(guildID, sp.ID); ok {
			candidates = append(candidates, candidate{sp, channelID})
		}
	}

	resultCh := make(chan speakerResult, len(candidates))
	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for _, c := range candidates {
		go func(sp *domain.Speaker, channelID snowflake.ID) {
			defer wg.Done()
			if err := m.poolSvc.JoinChannel(ctx, guildID, sp.ID, channelID); err != nil {
				slog.Warn("speaker failed to join channel", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				return
			}
			chOut := make(chan []byte, audioChanBuf)
			var chCapture chan []byte
			if withCapture {
				chCapture = make(chan []byte, audioChanBuf)
			}
			if err := m.consumeSpeaker(ctx, sp.ID, guildID, chOut, chCapture); err != nil {
				slog.Error("failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				m.poolSvc.LeaveChannel(ctx, guildID, sp.ID)
				return
			}
			resultCh <- speakerResult{sp, chOut, chCapture, channelID}
		}(c.speaker, c.channelID)
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
func (m *Service) commitSession(guildID snowflake.ID, session *domain.VoiceSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statuses[guildID]
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
// chOut is the provider channel (frames to play). chCapture, when non-nil,
// receives frames captured from the speaker's channel; nil disables capture.
func (m *Service) consumeSpeaker(ctx context.Context, speakerID, guildID snowflake.ID, chOut <-chan []byte, chCapture chan<- []byte) error {
	client, ok := m.poolSvc.GetClientByID(speakerID)
	if !ok {
		return fmt.Errorf("speaker %s is not in the pool", speakerID)
	}
	conn := client.VoiceManager.GetConn(guildID)
	if conn == nil {
		return fmt.Errorf("speaker %s is not connected to a voice channel in guild %s", speakerID, guildID)
	}

	var provider voice.OpusFrameProvider
	if m.test.IsTestBot(speakerID) {
		fp, err := opus.NewFileVoiceProvider(m.test.FileDCA)
		if err != nil {
			return fmt.Errorf("open dca file: %w", err)
		}
		if chCapture != nil {
			// Tee file audio into chCapture so it feeds the relay mixer.
			provider = opus.NewTeeProvider(fp, chCapture)
			chCapture = nil
		} else {
			provider = fp
		}
		go func() {
			for range chOut {
			}
		}()
	} else {
		provider = opus.NewVoiceProvider(chOut)
	}

	conn.SetOpusFrameProvider(provider)

	var receiver closer
	if chCapture != nil {
		r := opus.NewVoiceReceiver(chCapture, speakerID, nil)
		conn.SetOpusFrameReceiver(r)
		receiver = r
	} else {
		r := opus.NewEmptyVoiceReceiver()
		conn.SetOpusFrameReceiver(r)
		receiver = r
	}

	go func() {
		<-ctx.Done()
		provider.Close()
		receiver.Close()
	}()

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return fmt.Errorf("set speaking flag: %w", err)
	}
	return nil
}

// buildSources returns a deduplicated list of audio sources (one capture channel per voice
// channel). When two speaker bots share a channel the second capture is drained and discarded.
func buildSources(ownerUserID, ownerChannelID snowflake.ID, chIn chan []byte, joined []speakerResult) []sourceEntry {
	sources := []sourceEntry{{ownerUserID, ownerChannelID, chIn}}
	seenCapChannels := map[snowflake.ID]bool{}
	for _, r := range joined {
		if r.chCapture == nil {
			continue
		}
		if seenCapChannels[r.channelID] {
			// Second bot in same channel: drain and discard its capture to avoid doubling.
			go func(ch chan []byte) {
				for range ch {
				}
			}(r.chCapture)
			continue
		}
		seenCapChannels[r.channelID] = true
		sources = append(sources, sourceEntry{r.speaker.ID, r.channelID, r.chCapture})
	}
	return sources
}

// buildDestinations groups each speaker's output channel by its destination voice channel.
func buildDestinations(joined []speakerResult) []*destChannel {
	destMap := map[snowflake.ID]*destChannel{}
	for _, r := range joined {
		if _, ok := destMap[r.channelID]; !ok {
			destMap[r.channelID] = &destChannel{channelID: r.channelID}
		}
		destMap[r.channelID].outs = append(destMap[r.channelID].outs, r.chOut)
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

		go func(in chan []byte, targets []chan []byte) {
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
		go func(mx *opus.Mixer, destOuts []chan []byte) {
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
// Closes receiver and ownerProvider and logs when the context is cancelled.
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *relay.Session, ownerReceiver, ownerProvider closer, guildID snowflake.ID) {
	go relayMixer.Run(ctx)
	go func() {
		defer func() {
			ownerReceiver.Close()
			ownerProvider.Close()
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

// speakerResult holds the outcome of a single successfully joined speaker.
type speakerResult struct {
	speaker   *domain.Speaker
	chOut     chan []byte
	chCapture chan []byte // nil when withCapture is false
	channelID snowflake.ID
}

// setupOwnerCapture configures the owner connection for audio capture (host mode).
// The owner listens for caller-role-filtered voice frames and writes them into chIn.
// Returns chIn, receiver and provider so the caller can close them on teardown.
func (m *Service) setupOwnerCapture(ctx context.Context, conn voice.Conn, ownerUserID, guildID snowflake.ID) (chan []byte, *opus.VoiceReceiver, *opus.EmptyVoiceProvider, error) {
	caches := m.getOwnerClient().Caches

	var allowUser func(snowflake.ID) bool
	if roleID, ok := m.store.GetBoundRole(guildID, store.RoleTypeCaller); ok {
		slog.Info("role filter active", slog.String("guildID", guildID.String()), slog.String("roleID", roleID.String()))

		// Pre-fetch full member data for every user currently in the owner's voice
		// channel via a single RequestMembers gateway op. Discord responds with
		// GUILD_MEMBERS_CHUNK events that populate the cache with complete RoleIDs,
		// replacing any partial entries written by earlier VOICE_STATE_UPDATE events.
		if chID := conn.ChannelID(); chID != nil {
			var userIDs []snowflake.ID
			for vs := range caches.VoiceStates(guildID) {
				if vs.ChannelID != nil && *vs.ChannelID == *chID && vs.UserID != ownerUserID {
					userIDs = append(userIDs, vs.UserID)
				}
			}
			if len(userIDs) > 0 {
				if err := m.getOwnerClient().RequestMembers(ctx, guildID, false, "", userIDs...); err != nil {
					slog.Warn("setupOwnerCapture: RequestMembers failed", slog.Any("err", err))
				}
			}
		}

		allowUser = func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			if !ok {
				return false
			}
			if m.test.IsTestBot(userID) {
				return true
			}
			if member.User.Bot {
				return false
			}
			for _, rID := range member.RoleIDs {
				if rID == roleID {
					return true
				}
			}
			return false
		}
	} else {
		// No role filter — allow all non-bot users.
		allowUser = func(userID snowflake.ID) bool {
			member, ok := caches.Member(guildID, userID)
			return ok && !member.User.Bot
		}
	}

	chIn := make(chan []byte, audioChanBuf)
	receiver := opus.NewVoiceReceiver(chIn, ownerUserID, allowUser)
	provider := opus.NewEmptyVoiceProvider()
	conn.SetOpusFrameReceiver(receiver)
	conn.SetOpusFrameProvider(provider)
	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return nil, nil, nil, fmt.Errorf("set speaking flag: %w", err)
	}
	return chIn, receiver, provider, nil
}

// setupOwnerRelay configures the owner connection for audio relay (guest mode).
// The owner plays frames from chOut and discards any incoming voice.
// Returns chOut and provider so the caller can close them on teardown.
func (m *Service) setupOwnerRelay(ctx context.Context, conn voice.Conn) (chan []byte, *opus.VoiceProvider, error) {
	chOut := make(chan []byte, audioChanBuf)
	provider := opus.NewVoiceProvider(chOut)
	conn.SetOpusFrameProvider(provider)
	conn.SetOpusFrameReceiver(opus.NewEmptyVoiceReceiver())
	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return nil, nil, fmt.Errorf("set owner speaking flag: %w", err)
	}
	return chOut, provider, nil
}

// snapshotSpeakers returns a deep copy of the guild's speakers as a slice.
// Returns an error if the guild has no status or already has an active session.
func (m *Service) snapshotSpeakers(guildID snowflake.ID) ([]*domain.Speaker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.statuses[guildID]
	if st == nil {
		return nil, fmt.Errorf("no guild status found — seed the guild first")
	}
	if st.Session != nil {
		return nil, fmt.Errorf("a voice raid is already active in this server")
	}
	speakers := make([]*domain.Speaker, 0, len(st.Speakers))
	for _, v := range st.Speakers {
		speakers = append(speakers, new(*v))
	}
	return speakers, nil
}
