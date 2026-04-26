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

	// ErrNoSpeakers is returned when no speaker bots could join their bound
	// channels (all offline or unbound).
	ErrNoSpeakers = errors.New("no speakers joined: verify speaker channels are bound and bots are online in this guild")
)
