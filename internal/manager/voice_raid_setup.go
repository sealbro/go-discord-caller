package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

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
		return nil, ErrNoSpeakers
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
			chCapture, handle, cleanup, err := m.consumeSpeaker(ctx, guildID, sp.ID, conn, chOut, withCapture, allowUser)
			if err != nil {
				slog.ErrorContext(ctx, "failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				gv.Leave(ctx, guildID)
				return
			}
			m.storeApplier(guildID, sp.ID, m.buildApplier(guildID, sp.ID, chOut, chCapture, handle, allowUser))
			resultCh <- speakerResult{sp, chOut, chCapture, handle, gv, cleanup}
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
		return fmt.Errorf("commit session: guild status disappeared")
	}
	if st.HasActiveSession() {
		return ErrSessionExists
	}
	st.Session = session
	return nil
}

// consumeSpeaker sets up audio provider and receiver for a speaker's voice connection.
// chOut is the provider channel (frames to play). When withCapture is true the
// returned channel receives frames captured from the speaker's channel, filtered
// by allowUser (shared filter built once at session start), and the returned
// FanoutHandle must be passed to the topology wiring code so it can call Install.
// The caller is responsible for calling the returned cleanup function.
func (m *Service) consumeSpeaker(ctx context.Context, guildID, speakerID snowflake.ID, conn voice.Conn, chOut <-chan []byte, withCapture bool, allowUser func(snowflake.ID) bool) (chan []byte, *opus.FanoutHandle, func(), error) {
	gm := m.metrics.ForGuild(ctx, guildID)
	session := NewVoiceConnSetup(speakerID)
	if m.test.IsTestBot(speakerID) {
		session.WithFileProvider(m.test.FileDCA)
	} else {
		session.WithVoiceProvider(gm.Provider())
	}

	if withCapture {
		session.WithVoiceReceiver(allowUser, gm.Receiver())
	}

	capture, handle, cleanup, err := session.Apply(ctx, conn, chOut)
	if err != nil {
		return nil, nil, nil, err
	}

	return capture, handle, cleanup, nil
}

// iterDeduplicatedCaptures calls fn for the first capture channel per voice
// channel across joined. Any subsequent capture from the same channel is drained
// in a background goroutine to prevent the VoiceReceiver from blocking.
// ctx is threaded through so the drain goroutine exits promptly on cancellation
// rather than waiting for the channel to close.
func iterDeduplicatedCaptures(ctx context.Context, joined []speakerResult, fn func(speakerResult)) {
	seen := map[snowflake.ID]bool{}
	for _, r := range joined {
		if r.chCapture == nil {
			continue
		}
		if seen[r.gv.ChannelID()] {
			go func(ch <-chan []byte) {
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							return
						}
					case <-ctx.Done():
						return
					}
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
func buildSources(ctx context.Context, ownerUserID, ownerChannelID snowflake.ID, chIn chan []byte, ownerHandle *opus.FanoutHandle, joined []speakerResult) []sourceEntry {
	sources := []sourceEntry{{ownerUserID, ownerChannelID, chIn, ownerHandle}}
	iterDeduplicatedCaptures(ctx, joined, func(r speakerResult) {
		sources = append(sources, sourceEntry{r.speaker.ID, r.gv.ChannelID(), r.chCapture, r.handle})
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

// buildGuestSources returns deduplicated capture sources from speaker joins.
// Unlike the host's buildSources, the guest owner bot is provider-only so
// it contributes no capture channel.
func buildGuestSources(ctx context.Context, joined []speakerResult) []sourceEntry {
	var sources []sourceEntry
	iterDeduplicatedCaptures(ctx, joined, func(r speakerResult) {
		sources = append(sources, sourceEntry{r.speaker.ID, r.gv.ChannelID(), r.chCapture, r.handle})
	})
	return sources
}

// buildSpeakerCleanup returns a function that closes every speaker's
// provider/receiver and leaves its voice channel, exactly once.
// Leave calls run in parallel so teardown is bounded by the slowest connection
// rather than N×voiceLeaveTimeout.
func buildSpeakerCleanup(guildID snowflake.ID, joined []speakerResult) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), voiceLeaveTimeout)
			defer cancel()
			var wg sync.WaitGroup
			wg.Add(len(joined))
			for _, r := range joined {
				go func(r speakerResult) {
					defer wg.Done()
					if r.cleanup != nil {
						r.cleanup()
					}
					r.gv.Leave(ctx, guildID)
				}(r)
			}
			wg.Wait()
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
		return nil, ErrNoGuildStatus
	}
	if st.Session != nil {
		return nil, ErrSessionExists
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
