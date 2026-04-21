package guild

// RaidMode defines how a voice raid captures and relays audio.
//
// Three caller-mode choices map to concrete modes depending on whether the
// guild is the host or a guest:
//
//	callerModeOne     → host: RaidModeOneCaller           / guest: RaidModeAllyListener
//	callerModeMany    → host: RaidModeGuildCaller         / guest: RaidModeAllyCaller
//	callerModeOneMany → host: RaidModeOneManyGuildCaller  / guest: RaidModeOneManyAllyCaller
//
// Full behaviour matrix:
//
//	Scenario             Host mode              Guest mode             Speakers   Cross-server   Guests
//	                                                                   capture?   relay?         contribute?
//	──────────────────────────────────────────────────────────────────────────────────────────────────────────
//	Host, one caller     OneCaller              —                      ✗          ✓ (code gen)   ✗
//	Host, many callers   GuildCaller            —                      ✓          ✓ (code gen)   ✓
//	Host, one↔many       OneManyGuildCaller     —                      ✓          ✓ (code gen)   ✓
//	Guest, listener      —                      AllyListener           ✗          receives only  ✗
//	Guest, ally          —                      AllyCaller*            ✓          recv + send    ✓
//	Guest, one↔many      —                      OneManyAllyCaller*     ✓          recv + send    ✓
//
//	* AllyCaller/OneManyAllyCaller are only effective when the host allows guest
//	  capture; they are automatically downgraded to AllyListener otherwise.
//
// Star topology (one↔many): The owner is the central hub. Owner hears all
// speakers, but each speaker only hears the owner — speakers cannot hear each
// other. In guest mode, all captures go to the relay (reaching the host owner)
// and all channels receive the relay (host owner's voice).
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

	// RaidModeOneManyGuildCaller (Host) uses a star topology: the owner bot is
	// the central hub that hears all speakers, but each speaker only hears the
	// owner — speakers cannot hear each other. Speakers capture audio and send
	// it only to the owner's channel mixer; the owner's audio fans out to all
	// speaker channel mixers. The relay mixer broadcasts all sources to guests.
	// Mapped from: host + callerModeOneMany.
	RaidModeOneManyGuildCaller RaidMode = "one_many_guild_caller"

	// RaidModeOneManyAllyCaller (Guest) uses the star topology in guest mode:
	// all captures go to the relay mixer only (reaching the host owner via
	// broadcast) and all channels receive the relay from the host (owner's
	// voice). Guest speakers cannot hear each other locally — only the host
	// owner's relayed audio reaches them. Downgraded to RaidModeAllyListener
	// when the host does not allow guest capture.
	// Mapped from: guest + callerModeOneMany.
	RaidModeOneManyAllyCaller RaidMode = "one_many_ally_caller"
)

// WithCapture reports whether speaker bots in this guild should capture audio
// from their channels in addition to relaying.
func (m RaidMode) WithCapture() bool {
	return m == RaidModeGuildCaller || m == RaidModeAllyCaller ||
		m == RaidModeOneManyGuildCaller || m == RaidModeOneManyAllyCaller
}

// AllowGuestCapture reports whether this host mode permits guest guilds to
// capture audio from their own channels and contribute it to the relay mixer.
// Only RaidModeGuildCaller and RaidModeOneManyGuildCaller allow this; guests
// are downgraded to RaidModeAllyListener otherwise.
func (m RaidMode) AllowGuestCapture() bool {
	return m == RaidModeGuildCaller || m == RaidModeOneManyGuildCaller
}

// IsStarTopology reports whether this mode uses the one↔many star topology
// where the owner is the hub and speakers are isolated from each other.
func (m RaidMode) IsStarTopology() bool {
	return m == RaidModeOneManyGuildCaller || m == RaidModeOneManyAllyCaller
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
	case RaidModeOneManyGuildCaller:
		return "One↔Many Callers (host)"
	case RaidModeOneManyAllyCaller:
		return "One↔Many Caller (guest)"
	default:
		return string(m)
	}
}
