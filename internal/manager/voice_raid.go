package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/domain"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/relay"
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
		m.speaker.LeaveChannel(ctx, guildID, sp.ID)
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
func (m *Service) StartVoiceRaid(ctx context.Context, cancelFunc context.CancelFunc, guildID snowflake.ID, mode domain.RaidMode) (relay.RelayCode, error) {
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return "", err
	}

	ownerUser, ok := m.ownerClient.Caches.SelfUser()
	if !ok {
		return "", fmt.Errorf("owner bot self-user not yet cached")
	}
	if err := m.JoinChannel(ctx, guildID, ownerUser.ID); err != nil {
		return "", fmt.Errorf("failed to join owner channel: %w", err)
	}
	conn := m.ownerClient.VoiceManager.GetConn(guildID)
	if conn == nil {
		return "", fmt.Errorf("no voice connection to owner channel")
	}

	chIn, receiver, ownerProvider, err := m.setupOwnerCapture(ctx, conn, ownerUser.ID, guildID)
	if err != nil {
		return "", err
	}

	joinedSpeakers, outs, captures, channelIDs := m.joinSpeakers(ctx, guildID, speakers, mode.WithCapture())

	ownerChannelID, _ := m.store.GetBoundChannel(guildID, ownerUser.ID)
	sources := buildSources(ownerUser.ID, ownerChannelID, chIn, joinedSpeakers, captures, channelIDs)
	destinations := buildDestinations(joinedSpeakers, outs, channelIDs)

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
		slog.Int("activeSpeakers", len(joinedSpeakers)),
	)

	startChannelMixers(ctx, destinations, channelMixers)
	startRelayBroadcast(ctx, relayMixer, relaySession, receiver, ownerProvider, guildID)

	return relayCode, nil
}

// buildSources returns a deduplicated list of audio sources (one capture channel per voice
// channel). When two speaker bots share a channel the second capture is drained and discarded.
func buildSources(
	ownerUserID, ownerChannelID snowflake.ID,
	chIn chan []byte,
	joinedSpeakers []*domain.Speaker,
	captures []chan []byte,
	channelIDs []snowflake.ID,
) []sourceEntry {
	sources := []sourceEntry{{ownerUserID, ownerChannelID, chIn}}
	seenCapChannels := map[snowflake.ID]bool{}
	for i, sp := range joinedSpeakers {
		if captures[i] == nil {
			continue
		}
		chID := channelIDs[i]
		if seenCapChannels[chID] {
			// Second bot in same channel: drain and discard its capture to avoid doubling.
			go func(ch chan []byte) {
				for range ch {
				}
			}(captures[i])
			continue
		}
		seenCapChannels[chID] = true
		sources = append(sources, sourceEntry{sp.ID, chID, captures[i]})
	}
	return sources
}

// buildDestinations groups each speaker's output channel by its destination voice channel.
func buildDestinations(joinedSpeakers []*domain.Speaker, outs []chan []byte, channelIDs []snowflake.ID) []*destChannel {
	destMap := map[snowflake.ID]*destChannel{}
	for i := range joinedSpeakers {
		chID := channelIDs[i]
		if _, ok := destMap[chID]; !ok {
			destMap[chID] = &destChannel{channelID: chID}
		}
		destMap[chID].outs = append(destMap[chID].outs, outs[i])
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
func startRelayBroadcast(ctx context.Context, relayMixer *opus.Mixer, relaySession *relay.Session, receiver, ownerProvider closer, guildID snowflake.ID) {
	go relayMixer.Run(ctx)
	go func() {
		defer func() {
			receiver.Close()
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
