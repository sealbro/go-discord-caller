package guild

// RaidMode defines how a voice raid captures and relays audio.
//
// Two caller-mode choices map to four concrete modes depending on whether the
// guild is the host or a guest:
//
//	callerModeOne  → host: RaidModeOneCaller  / guest: RaidModeAllyListener
//	callerModeMany → host: RaidModeGuildCaller / guest: RaidModeAllyCaller
//
// Full behaviour matrix:
//
//	Scenario          Host mode     Guest mode   Speakers   Cross-server   Guests
//	                                             capture?   relay?         contribute?
//	────────────────────────────────────────────────────────────────────────────────
//	Host, one caller  OneCaller     —            ✗          ✓ (code gen)   ✗
//	Host, many calls  GuildCaller   —            ✓          ✓ (code gen)   ✓
//	Guest, listener   —             GuestOne     ✗          receives only  ✗
//	Guest, ally       —             AllyCaller*  ✓          recv + send    ✓
//
//	* AllyCaller is only effective when the host uses GuildCaller; it is
//	  automatically downgraded to GuestOne when the host uses OneCaller.
type RaidMode string

const (
	// RaidModeOneCaller (Host) captures audio from users with the capture role
	// in the owner bot's channel and relays it to all bound speakers in
	// real-time. Other speakers cannot capture audio from their channels.
	// Mapped from: host + callerModeOne.
	RaidModeOneCaller RaidMode = "one_caller"

	// RaidModeGuildCaller (Host) captures audio from users with the capture
	// role in all channels (owner + speakers), mixes the audio and relays it
	// to all bound speakers in real-time. This allows multi-directional voice
	// relay between multiple channels within the guild.
	// Mapped from: host + callerModeMany.
	RaidModeGuildCaller RaidMode = "guild_caller"

	// RaidModeAllyListener (Guest) — the host captures audio from users with the
	// capture role in the owner bot's channel and relays it to all bound
	// speakers in real-time, but also fans out the captured audio to any guest
	// guilds that joined using the relay code, allowing cross-server voice
	// relay. In this mode only the host's owner bot guild captures audio;
	// guest guilds are listeners only.
	// Mapped from: guest + callerModeOne.
	RaidModeAllyListener RaidMode = "ally_listener"

	// RaidModeAllyCaller (Guest) — only effective when the host uses
	// callerModeMany (RaidModeGuildCaller). Captures audio from users with the
	// capture role in all channels (owner + speakers), mixes the audio and
	// relays it to all bound speakers in real-time, but also fans out the
	// captured audio to any guest guilds that joined using the relay code,
	// allowing multi-directional cross-server voice relay between multiple
	// channels across multiple guilds simultaneously. Downgraded to
	// RaidModeAllyListener when the host uses callerModeOne.
	// Mapped from: guest + callerModeMany.
	RaidModeAllyCaller RaidMode = "ally_caller"
)

// WithCapture reports whether speaker bots in this guild should capture audio
// from their channels in addition to relaying.
func (m RaidMode) WithCapture() bool {
	return m == RaidModeGuildCaller || m == RaidModeAllyCaller
}

// AllowGuestCapture reports whether this host mode permits guest guilds to
// capture audio from their own channels and contribute it to the relay mixer.
// Only RaidModeGuildCaller (host many-callers) allows this; guests that join
// with RaidModeAllyCaller are downgraded to RaidModeAllyListener otherwise.
func (m RaidMode) AllowGuestCapture() bool {
	return m == RaidModeGuildCaller
}

// Pretty returns a human-readable label for use in Discord messages.
func (m RaidMode) Pretty() string {
	switch m {
	case RaidModeOneCaller:
		return "One Caller (host)"
	case RaidModeGuildCaller:
		return "Many Callers (host)"
	case RaidModeAllyListener:
		return "Listener (guest)"
	case RaidModeAllyCaller:
		return "Caller (guest)"
	default:
		return string(m)
	}
}
