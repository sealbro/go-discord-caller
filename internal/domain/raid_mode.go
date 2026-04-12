package domain

// RaidMode defines how a voice raid captures and relays audio.
type RaidMode string

const (
	// RaidModeOneCaller captures audio only from the owner bot's channel.
	// Speaker bots relay but do not capture. Valid for host (/start without code).
	RaidModeOneCaller RaidMode = "one"

	// RaidModeGuildCaller captures audio from the owner bot's channel and all
	// speaker channels, mixes them, and relays between channels.
	// Valid for host (/start without code).
	RaidModeGuildCaller RaidMode = "guild"

	// RaidModeGuestOne joins an existing relay session as a listener only.
	// Guest speakers receive and play host audio but do not capture their channels.
	// Valid for guest (/start with code).
	RaidModeGuestOne RaidMode = "guest_one"

	// RaidModeAllyCaller joins an existing relay session as an active participant.
	// Guest speakers receive host audio AND capture from their own channels,
	// enabling multi-directional voice relay across servers.
	// Valid for guest (/start with code).
	RaidModeAllyCaller RaidMode = "ally"
)

// WithCapture reports whether speaker bots should also capture audio from their channels.
func (m RaidMode) WithCapture() bool {
	return m == RaidModeGuildCaller || m == RaidModeAllyCaller
}
