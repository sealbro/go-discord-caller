// Package backend builds the godave.SessionCreateFunc for the selected DAVE
// implementation. It is split out from internal/dave because it imports
// golibdave, and therefore CGO and libdave headers — which would otherwise
// spread to every package that only needs to read the DAVE_IMPL setting.
package backend

import (
	"log/slog"

	"github.com/disgoorg/godave"
	"github.com/disgoorg/godave/golibdave"
	"github.com/sealbro/go-discord-caller/internal/dave"
	davego "github.com/thomas-vilte/dave-go/session"
)

// SessionCreateFunc returns the factory for impl, ready to hand to
// voice.WithDaveSessionCreateFunc.
//
// For dave-go the factory registers a session hook that files every session in
// reg, which is what gives the process a handle on sessions disgo otherwise
// keeps to itself — needed both to export their counters and to Close them
// when the connection goes away. reg may be nil, in which case sessions are
// simply not tracked.
//
// An unrecognised Impl falls back to the default rather than returning nil,
// because a nil create func would leave disgo without any DAVE session and
// break voice outright.
func SessionCreateFunc(impl dave.Impl, reg *dave.Registry) godave.SessionCreateFunc {
	switch impl {
	case dave.ImplDaveGo:
		return daveGoCreateFunc(reg)
	case dave.ImplLibdave:
		return golibdave.NewSession
	default:
		return golibdave.NewSession
	}
}

// daveGoCreateFunc wraps dave-go's factory so each session is tracked in reg.
//
// The hook fires with the session already built but not yet returned to the
// voice layer, so tracking happens before any frame can flow through it. The
// key is the godave.Callbacks value disgo passes in, which is the voice
// connection itself — see dave.Registry for why that is the handle Release
// needs.
func daveGoCreateFunc(reg *dave.Registry) godave.SessionCreateFunc {
	return func(logger *slog.Logger, userID godave.UserID, callbacks godave.Callbacks) godave.Session {
		var tracked *davego.Session
		create := davego.CreateFunc(davego.WithSessionHook(func(s *davego.Session) {
			tracked = s
		}))
		sess := create(logger, userID, callbacks)
		reg.Track(callbacks, tracked)
		return sess
	}
}
