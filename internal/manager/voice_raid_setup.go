package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/pipeline"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/store"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// setupSpeakers snapshots and joins all enabled, bound speakers for a guild.
// Returns an error if the guild has no status, already has an active session,
// or no speakers could join.
func (m *Service) setupSpeakers(ctx context.Context, guildID snowflake.ID, mode guild.RaidMode, allowUser func(snowflake.ID) bool) (*pipeline.Setup, error) {
	speakers, err := m.snapshotSpeakers(guildID)
	if err != nil {
		return nil, err
	}

	// Separate "nothing configured" from "configured but nothing connected" —
	// the two have different remedies, so they get different errors.
	candidates := boundSpeakers(m.store, guildID, speakers)
	if len(candidates) == 0 {
		return nil, ErrNoBoundSpeakers
	}

	joined := m.joinSpeakers(ctx, guildID, candidates, mode.WithCapture(), allowUser)
	if len(joined) == 0 {
		return nil, ErrNoSpeakers
	}

	outs := make([]chan<- []byte, 0, len(joined))
	joinedSpeakers := make([]guild.Speaker, 0, len(joined))
	for _, r := range joined {
		outs = append(outs, r.ChOut)
		joinedSpeakers = append(joinedSpeakers, r.Speaker)
	}

	return &pipeline.Setup{
		Joined:         joined,
		Speakers:       joinedSpeakers,
		SpeakerCleanup: pipeline.BuildSpeakerCleanup(guildID, joined),
		Outs:           outs,
	}, nil
}

// boundSpeakers returns the speakers eligible to join: enabled AND bound to a
// voice channel in this guild. An empty result means the guild was never set up
// (see ErrNoBoundSpeakers), not that a join failed.
func boundSpeakers(st store.Store, guildID snowflake.ID, speakers []guild.Speaker) []guild.Speaker {
	var candidates []guild.Speaker
	for _, sp := range speakers {
		if !sp.Enabled {
			continue
		}
		if _, ok := st.GetBoundChannel(guildID, sp.ID); !ok {
			continue
		}
		candidates = append(candidates, sp)
	}
	return candidates
}

// joinSpeakers joins the given candidate speakers in parallel; callers filter
// with boundSpeakers first.
// When withCapture is true each speaker also captures incoming frames, filtered by allowUser.
func (m *Service) joinSpeakers(ctx context.Context, guildID snowflake.ID, candidates []guild.Speaker, withCapture bool, allowUser func(snowflake.ID) bool) []pipeline.SpeakerResult {
	resultCh := make(chan pipeline.SpeakerResult, len(candidates))
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
				// Drop the half-open conn. Join failures are usually Open
				// timeouts, and disgo leaves the conn registered in its voice
				// manager: the handshake may still complete afterwards, leaving
				// the bot parked in the channel with no provider/receiver and a
				// stale token buffered in the conn's openedChan. CreateConn
				// hands that same conn to the next session, where Open then
				// returns instantly off the stale token without being connected.
				gv.Leave(ctx, guildID)
				return
			}
			if withCapture {
				m.prefetchChannelMembers(ctx, conn, sp.ID, guildID)
			}
			chOut := make(chan []byte, opus.AudioChanBuf)
			handle, cleanup, err := m.consumeSpeaker(ctx, guildID, sp.ID, conn, chOut, withCapture, allowUser)
			if err != nil {
				slog.ErrorContext(ctx, "failed to consume voice data", slog.String("speakerID", sp.ID.String()), slog.Any("err", err))
				gv.Leave(ctx, guildID)
				return
			}
			m.storeApplier(guildID, sp.ID, m.buildApplier(guildID, sp.ID, chOut, handle, allowUser))
			resultCh <- pipeline.SpeakerResult{Speaker: sp, ChOut: chOut, Handle: handle, GV: gv, Cleanup: cleanup}
		}(sp)
	}
	wg.Wait()
	close(resultCh)

	results := make([]pipeline.SpeakerResult, 0, len(candidates))
	for r := range resultCh {
		results = append(results, r)
	}
	return results
}

// commitSession stores session under write lock, re-checking for conflicts.
// Also publishes the session's AutoRouter into the lock-free activeRouters
// snapshot so voice-event handlers can dispatch via AutoRoute without
// taking m.mu.
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
	if session.AutoRouter != nil {
		m.setActiveRouter(session.GuildID, session.AutoRouter)
	}
	return nil
}

// consumeSpeaker sets up audio provider and receiver for a speaker's voice connection.
// chOut is the provider channel (frames to play). When withCapture is true the
// receiver decodes incoming frames inline via the returned FanoutHandle, which
// the topology wiring code must Install with mixer/raw targets.
// The caller is responsible for calling the returned cleanup function.
func (m *Service) consumeSpeaker(ctx context.Context, guildID, speakerID snowflake.ID, conn voice.Conn, chOut <-chan []byte, withCapture bool, allowUser func(snowflake.ID) bool) (*opus.FanoutHandle, func(), error) {
	gm := m.metrics.ForGuild(ctx, guildID)
	session := NewVoiceConnSetup(speakerID)
	session.WithVoiceProvider(gm.Provider())

	if withCapture {
		session.WithVoiceReceiver(allowUser, gm.Receiver())
	}

	handle, cleanup, err := session.Apply(ctx, conn, chOut)
	if err != nil {
		return nil, nil, err
	}
	// Repair the pipeline if disgo silently re-identifies this voice gateway;
	// that swaps the UDP socket out from under the sender/receiver.
	m.watchVoiceReady(guildID, speakerID, conn)

	return handle, cleanup, nil
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
