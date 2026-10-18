package pool

import (
	"testing"

	"github.com/disgoorg/disgo/voice"
)

type gatewayConn struct {
	voice.Conn
	gw voice.Gateway
}

func (c gatewayConn) Gateway() voice.Gateway { return c.gw }

type fakeGateway struct {
	voice.Gateway
	name string
}

func TestVoiceReadyRoutesByGateway(t *testing.T) {
	t.Parallel()

	w := newVoiceReadyWatchers()
	gwA, gwB := &fakeGateway{name: "a"}, &fakeGateway{name: "b"}
	connA, connB := gatewayConn{gw: gwA}, gatewayConn{gw: gwB}

	var readyA, readyB int
	w.watch(connA, func() { readyA++ })
	w.watch(connB, func() { readyB++ })

	w.handle(gwA, voice.OpcodeReady, 0, nil)
	w.handle(gwA, voice.OpcodeSpeaking, 0, nil)
	if readyA != 1 || readyB != 0 {
		t.Fatalf("after one Ready on A: a=%d b=%d, want a=1 b=0", readyA, readyB)
	}

	w.forget(connA)
	w.handle(gwA, voice.OpcodeReady, 0, nil)
	w.handle(gwB, voice.OpcodeReady, 0, nil)
	if readyA != 1 || readyB != 1 {
		t.Fatalf("after forgetting A: a=%d b=%d, want a=1 b=1", readyA, readyB)
	}
}
