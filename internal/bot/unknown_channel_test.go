package bot

import (
	"encoding/json"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
)

const unknownChannelJSON = `{"id":"300","guild_id":"100","type":20,"name":"new-kind","position":0,"permission_overwrites":[]}`

// A channel type disgo has no struct for must not fail the whole payload: on
// 2026-11-07 a type 20 CHANNEL_CREATE broke the gateway read, and every
// reconnect re-ran startup seeding, which failed on the same channel list.
func TestGatewayAcceptsUnknownChannelType(t *testing.T) {
	data, err := gateway.UnmarshalEventData([]byte(unknownChannelJSON), gateway.EventTypeChannelCreate)
	if err != nil {
		t.Fatalf("CHANNEL_CREATE with an unknown channel type must parse, got: %v", err)
	}
	if created, ok := data.(gateway.EventChannelCreate); !ok || created.ID() != 300 {
		t.Fatalf("unexpected event data %#v", data)
	}
}

func TestChannelListAcceptsUnknownChannelType(t *testing.T) {
	var list []discord.UnmarshalChannel
	payload := `[{"id":"200","guild_id":"100","type":2,"name":"voice"},` + unknownChannelJSON + `]`
	if err := json.Unmarshal([]byte(payload), &list); err != nil {
		t.Fatalf("guild channel list with an unknown channel type must parse, got: %v", err)
	}
}
