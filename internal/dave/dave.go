// Package dave selects the DAVE (Discord end-to-end encrypted voice)
// implementation used by every voice connection this bot opens.
//
// Two implementations satisfy godave.Session and are wired identically into
// disgo's voice manager:
//
//   - golibdave — CGO binding around discord/libdave, Discord's own reference
//     implementation. Default, and what every release up to now has shipped.
//   - dave-go — pure Go implementation on top of an RFC 9420 MLS library.
//
// The switch exists because the two behave differently when the MLS group
// re-keys, which is what happens every time a user joins or leaves the voice
// channel. golibdave tears the crypto state down in OnDavePrepareEpoch and
// installs a key ratchet wrapping a NULL handle until the new Welcome arrives,
// so audio fails for the duration of the handshake (disgoorg/godave#5, open);
// if the following commit is rejected the session can stay silent for minutes
// while the bot still looks connected. dave-go retains the previous send
// ratchet and previous epochs across that window instead.
//
// Both are selected per process, not per guild: the choice is a property of
// the binary's voice stack, and mixing them would make an outage impossible to
// attribute.
package dave

import (
	"fmt"
	"strings"

	"github.com/disgoorg/godave"
	"github.com/disgoorg/godave/golibdave"
	davego "github.com/thomas-vilte/dave-go/session"
)

// Impl identifies a DAVE session implementation.
type Impl string

const (
	// ImplLibdave is the CGO binding around Discord's libdave. Default.
	ImplLibdave Impl = "libdave"
	// ImplDaveGo is the pure Go implementation (github.com/thomas-vilte/dave-go).
	ImplDaveGo Impl = "dave-go"
)

// Default is the implementation used when DAVE_IMPL is unset.
const Default = ImplLibdave

// Parse maps a DAVE_IMPL value to an Impl. Empty input yields Default.
// Matching is case-insensitive and tolerates the "godave"/"davego" spellings
// people reach for first. Unknown values are an error; the caller decides
// whether to fall back or refuse to start.
func Parse(s string) (Impl, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return Default, nil
	case "libdave", "golibdave", "godave":
		return ImplLibdave, nil
	case "dave-go", "davego", "dave_go":
		return ImplDaveGo, nil
	default:
		return Default, fmt.Errorf("unknown DAVE implementation %q (want %q or %q)", s, ImplLibdave, ImplDaveGo)
	}
}

// SessionCreateFunc returns the godave.SessionCreateFunc for impl, ready to
// hand to voice.WithDaveSessionCreateFunc. An unrecognised Impl falls back to
// Default rather than returning nil, because a nil create func would leave
// disgo without any DAVE session and break voice outright.
func SessionCreateFunc(impl Impl) godave.SessionCreateFunc {
	switch impl {
	case ImplDaveGo:
		return davego.CreateFunc()
	case ImplLibdave:
		return golibdave.NewSession
	default:
		return golibdave.NewSession
	}
}
