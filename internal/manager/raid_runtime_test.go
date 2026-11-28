package manager

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/raid"
	"github.com/sealbro/go-discord-caller/internal/raid/pipeline"
)

// countingRuntime builds raid audio in-process and counts what it was asked
// to build.
type countingRuntime struct {
	raid.Local
	hosts, guests atomic.Int32
}

func (r *countingRuntime) BuildHost(ctx context.Context, p pipeline.Params) (*guild.Session, func(), error) {
	r.hosts.Add(1)
	return r.Local.BuildHost(ctx, p)
}

func (r *countingRuntime) BuildGuest(ctx context.Context, p pipeline.GuestParams) (*guild.Session, func(), func(), error) {
	r.guests.Add(1)
	return r.Local.BuildGuest(ctx, p)
}

func TestRaid_BuildsItsAudioThroughTheRuntime(t *testing.T) {
	for _, side := range raidSides {
		t.Run(side.name, func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			rt := &countingRuntime{}
			f.m.audio = rt

			cancel, err := side.start(f)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cancel()
			defer func() { _ = f.m.StopVoiceRaid(context.Background(), side.guildID) }()

			wantHosts, wantGuests := int32(1), int32(0)
			if side.guildID != testGuildID {
				wantHosts, wantGuests = 0, 1
			}
			if h, g := rt.hosts.Load(), rt.guests.Load(); h != wantHosts || g != wantGuests {
				t.Errorf("runtime built %d host and %d guest graphs, want %d and %d", h, g, wantHosts, wantGuests)
			}
		})
	}
}
