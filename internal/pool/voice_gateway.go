package pool

import (
	"context"
	"errors"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
)

// ErrNoVoiceChannel is returned when a voice gateway is opened for a connection
// whose channel is already gone.
var ErrNoVoiceChannel = errors.New("voice gateway open with no channel")

// safeGateway refuses an open whose channel is already gone. The panic happens
// on disgo's own goroutine, so the guard cannot live in a caller.
//
// UPSTREAM(disgo v0.19.3): gateway Open dereferences a nil ChannelID
// (voice/gateway.go:183), which disgo nils itself when the bot leaves voice.
type safeGateway struct {
	voice.Gateway
}

func (g *safeGateway) Open(ctx context.Context, state voice.State) error {
	if state.ChannelID == nil {
		return ErrNoVoiceChannel
	}
	return g.Gateway.Open(ctx, state)
}

// NewSafeGateway is a voice.GatewayCreateFunc that builds disgo's voice gateway
// behind the safeGateway guard.
func NewSafeGateway(daveSession godave.Session, eventHandlerFunc voice.EventHandlerFunc, closeHandlerFunc voice.CloseHandlerFunc, opts ...voice.GatewayConfigOpt) voice.Gateway {
	return &safeGateway{Gateway: voice.NewGateway(daveSession, eventHandlerFunc, closeHandlerFunc, opts...)}
}

// SafeGatewayOpt installs NewSafeGateway on a bot's voice manager. Every client
// must pass it, same as SafeUDPConnOpt.
func SafeGatewayOpt() voice.ManagerConfigOpt {
	return voice.WithConnConfigOpts(voice.WithConnGatewayCreateFunc(NewSafeGateway))
}
