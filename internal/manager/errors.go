package manager

import "errors"

// Sentinel errors returned by session lifecycle methods.
// Callers can use errors.Is to distinguish expected conditions from unexpected
// failures and tailor their user-facing response accordingly.
var (
	// ErrNoActiveSession is returned when an operation requires an active voice
	// raid but none exists for the guild.
	ErrNoActiveSession = errors.New("no active voice raid in this server")

	// ErrSessionExists is returned when starting or joining a session while one
	// is already active in the guild.
	ErrSessionExists = errors.New("a voice raid is already active in this server")

	// ErrNoGuildStatus is returned when operating on a guild that has not been
	// seeded yet.
	ErrNoGuildStatus = errors.New("no guild status found — seed the guild first")

	// ErrNoSpeakers is returned when speaker bots were configured for this
	// guild but none of them managed to join its bound channel — offline
	// gateway, missing channel permissions, or a voice timeout.
	ErrNoSpeakers = errors.New("no speakers joined: verify speaker channels are bound and bots are online in this guild")

	// ErrNoBoundSpeakers is returned when the guild has no speaker that is both
	// enabled AND bound to a voice channel, so no join was even attempted.
	// Distinct from ErrNoSpeakers because the remedy is completely different:
	// this one is "you never configured this server", not "something failed".
	//
	// It is the normal outcome for a guest guild that joined with a relay code
	// but was never set up — bindings are per-guild, and the code carries none
	// of the host's configuration. CheckGuildChannelAccess cannot catch it
	// first: it skips speakers with no bound channel, so an entirely unbound
	// guild passes the permission pre-flight cleanly.
	ErrNoBoundSpeakers = errors.New("no speaker bots are configured in this server: run /setup → Bind Speakers to enable a speaker and pick its voice channel")
)
