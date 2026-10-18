package pool

import (
	"context"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
)

// serialGateway never lets a Send and a Close of disgo's voice gateway overlap.
//
// UPSTREAM(disgo 2d889db): Send holds the status lock while it waits for the
// connection lock, and CloseWithCode holds the connection lock while it waits
// for the status lock, so the two deadlock. A DAVE epoch arriving while the bot
// leaves voice does exactly that, and wedges the bot's main gateway with it.
// disgo's own heartbeat sends past this wrapper and can still race a Close.
// Fixed upstream by https://github.com/disgoorg/disgo/pull/588, unmerged as of
// 2026-10-18: once a disgo release includes it, delete this wrapper and confirm
// TestLeaveDoesNotDeadlockWithDaveEpoch still passes without it.
type serialGateway struct {
	voice.Gateway
	mu sync.Mutex
}

func (g *serialGateway) Send(ctx context.Context, op voice.Opcode, d voice.GatewayMessageData) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Gateway.Send(ctx, op, d)
}

func (g *serialGateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Gateway.Close()
}

func (g *serialGateway) CloseWithCode(code int, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Gateway.CloseWithCode(code, message)
}

// newSerialGateway is a voice.GatewayCreateFunc. disgo hands its handlers the
// inner gateway; they get the wrapper instead, since that is what conn.Gateway
// returns and what voiceReadyWatchers keys on.
func newSerialGateway(daveSession godave.Session, eventHandlerFunc voice.EventHandlerFunc, closeHandlerFunc voice.CloseHandlerFunc, opts ...voice.GatewayConfigOpt) voice.Gateway {
	g := &serialGateway{}
	g.Gateway = voice.NewGateway(daveSession,
		func(_ voice.Gateway, op voice.Opcode, seq int, d voice.GatewayMessageData) {
			eventHandlerFunc(g, op, seq, d)
		},
		func(_ voice.Gateway, err error) {
			closeHandlerFunc(g, err)
		},
		opts...)
	return g
}

// SerialGatewayOpt installs serialGateway on a bot's voice manager. Every client
// must pass it, same as SafeAudioSenderOpt.
func SerialGatewayOpt() voice.ManagerConfigOpt {
	return voice.WithConnConfigOpts(voice.WithConnGatewayCreateFunc(newSerialGateway))
}
