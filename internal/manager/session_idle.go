package manager

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// sessionPresenceInterval is how often the watcher checks whether the session's
// channels still have users. The auto-stop decision only needs minute-level
// granularity, so a 1-minute poll keeps the cost negligible.
const sessionPresenceInterval = time.Minute

// idleStopTimeout bounds the auto-stop teardown. The session context is torn
// down inside it, so the stop cannot borrow the session's own context.
const idleStopTimeout = 30 * time.Second

// sessionPresenceWatcher stops a voice raid session once every watched voice
// channel has been empty of non-bot users continuously for idleTimeout.
//
// It replaces the previous audio-activity approach: rather than inferring "idle"
// from whether mixers are consuming Opus frames, it asks Discord directly — if
// nobody is connected to any of the session's channels, the session is unused
// and gets stopped regardless of whether anyone was speaking.
type sessionPresenceWatcher struct {
	guildID  snowflake.ID
	channels []snowflake.ID
	probe    *cacheVoiceProbe
	// stop must run the full teardown, not just a context cancel: only
	// GuildVoice.Leave cancels the registered audio sender, and one left
	// behind polls its closed provider 50×/s for the life of the process.
	stop        func()
	idleTimeout time.Duration
}

// occupied reports whether any watched channel currently has a non-bot user.
func (w *sessionPresenceWatcher) occupied() bool {
	return slices.ContainsFunc(w.channels, w.probe.HasListeners)
}

// Run polls every sessionPresenceInterval until ctx is cancelled or until every
// watched channel has been empty continuously for w.idleTimeout — in which case
// it stops the session and returns. No-op when idleTimeout <= 0 or no channels
// are provided.
func (w *sessionPresenceWatcher) Run(ctx context.Context) {
	if w.idleTimeout <= 0 || len(w.channels) == 0 {
		return
	}
	interval := min(sessionPresenceInterval, w.idleTimeout)
	t := time.NewTicker(interval)
	defer t.Stop()

	var emptySince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.occupied() {
				emptySince = time.Time{}
				continue
			}
			if emptySince.IsZero() {
				emptySince = time.Now()
				continue
			}
			if time.Since(emptySince) >= w.idleTimeout {
				slog.Info("session idle: all channels empty, stopping voice raid",
					slog.String("guildID", w.guildID.String()),
					slog.Duration("idleTimeout", w.idleTimeout),
				)
				w.stop()
				return
			}
		}
	}
}
