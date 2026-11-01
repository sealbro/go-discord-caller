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

// occupiedReportInterval spaces the occupancy reports, so a long-lived raid does
// not log on every poll.
const occupiedReportInterval = 30 * time.Minute

// occupantCheckTimeout caps the Discord lookups behind one occupancy report so
// a slow API cannot hold up the idle poll.
const occupantCheckTimeout = 30 * time.Second

// occupancyProbe is what the watcher reads from the owner bot's view of voice.
type occupancyProbe interface {
	HasListeners(channelID snowflake.ID) bool
	Listeners(channelID snowflake.ID) []snowflake.ID
	EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID
	InChannel(ctx context.Context, channelID, userID snowflake.ID) (bool, error)
}

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
	probe    occupancyProbe
	// callerRole returns the bound caller role, or 0 when there is none.
	callerRole func() snowflake.ID
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

// hasCaller reports whether a role-bearing caller is in any watched channel: a
// session with callers is in use, so only one without them needs explaining.
func (w *sessionPresenceWatcher) hasCaller() bool {
	role := w.callerRole()
	return slices.ContainsFunc(w.channels, func(ch snowflake.ID) bool {
		return len(w.probe.EnumerateCallers(ch, role)) > 0
	})
}

// logOccupants reports, per occupied channel, how many listeners the cache sees
// and how many Discord confirms. Only counts leave the process: a cached listener
// Discord denies is a stale voice state, one it confirms is a real person.
func (w *sessionPresenceWatcher) logOccupants(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, occupantCheckTimeout)
	defer cancel()
	for _, ch := range w.channels {
		users := w.probe.Listeners(ch)
		if len(users) == 0 {
			continue
		}
		var confirmed, unverified int
		for _, user := range users {
			inChannel, err := w.probe.InChannel(ctx, ch, user)
			switch {
			case err != nil:
				unverified++
			case inChannel:
				confirmed++
			}
		}
		slog.Info("session idle: channel keeps the session alive",
			slog.String("guildID", w.guildID.String()),
			slog.String("channelID", ch.String()),
			slog.Int("cached", len(users)),
			slog.Int("confirmed", confirmed),
			slog.Int("unverified", unverified),
		)
	}
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

	var emptySince, lastReport time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.occupied() {
				emptySince = time.Time{}
				if !w.hasCaller() && time.Since(lastReport) >= occupiedReportInterval {
					lastReport = time.Now()
					w.logOccupants(ctx)
				}
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
