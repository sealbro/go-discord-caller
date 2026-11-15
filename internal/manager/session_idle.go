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

// occupancyProbe is what the watcher reads from the owner bot's view of voice.
type occupancyProbe interface {
	// EnumerateCallers lists callers in the channel, muted or not.
	EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID
	// EnumerateUnmutedCallers lists the callers who are not self-muted or
	// server-muted.
	EnumerateUnmutedCallers(channelID, roleID snowflake.ID) []snowflake.ID
	// Listeners lists every non-bot member in the channel.
	Listeners(channelID snowflake.ID) []snowflake.ID
}

// idleStopTimeout bounds the auto-stop teardown. The session context is torn
// down inside it, so the stop cannot borrow the session's own context.
const idleStopTimeout = 30 * time.Second

// sessionPresenceWatcher stops a voice raid session once it has gone idleTimeout
// without anything to relay: no unmuted role-bearing caller in a watched voice
// channel, or no one but that caller there to hear them.
//
// Without a caller nothing is captured, so every speaker is deafened and nobody
// is relayed; a muted caller sends nothing; and a caller alone in the channels is
// heard by no one. Other members do not keep a session alive by themselves.
//
// A session that shares its audio with other servers is judged on callers alone,
// muted or not: their members are out of this bot's sight, so who is speaking and
// who is listening there cannot be told from here.
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
	// sharesRelay reports whether other servers are attached to this session.
	// Nil means none.
	sharesRelay func() bool
}

// hasCaller reports whether a role-bearing caller is in any watched channel.
func (w *sessionPresenceWatcher) hasCaller() bool {
	role := w.callerRole()
	return slices.ContainsFunc(w.channels, func(ch snowflake.ID) bool {
		return len(w.probe.EnumerateCallers(ch, role)) > 0
	})
}

// hasAudience reports whether an unmuted caller has someone else to be heard by
// in the watched channels. The someone is any other human: one in the caller's
// own channel counts too, so the answer errs toward keeping a session alive.
func (w *sessionPresenceWatcher) hasAudience() bool {
	role := w.callerRole()
	var speakers, humans int
	for _, ch := range w.channels {
		speakers += len(w.probe.EnumerateUnmutedCallers(ch, role))
		humans += len(w.probe.Listeners(ch))
	}
	return speakers > 0 && humans > 1
}

// busy reports whether the session still has something to relay.
func (w *sessionPresenceWatcher) busy() bool {
	if w.sharesRelay != nil && w.sharesRelay() {
		return w.hasCaller()
	}
	return w.hasAudience()
}

// Run polls every sessionPresenceInterval until ctx is cancelled or until no
// watched channel has had a caller continuously for w.idleTimeout — in which case
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
			if w.busy() {
				emptySince = time.Time{}
				continue
			}
			if emptySince.IsZero() {
				emptySince = time.Now()
				continue
			}
			if time.Since(emptySince) >= w.idleTimeout {
				slog.Info("session idle: no one speaking to a listener, stopping voice raid",
					slog.String("guildID", w.guildID.String()),
					slog.Duration("idleTimeout", w.idleTimeout),
				)
				w.stop()
				return
			}
		}
	}
}
