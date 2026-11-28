package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
)

// watchRecorder is a DeafSink that records the capture sources it is handed.
type watchRecorder struct {
	mu      sync.Mutex
	watched []map[snowflake.ID]snowflake.ID
}

func (*watchRecorder) Register(snowflake.ID, bool) {}
func (*watchRecorder) Close(context.Context)       {}
func (w *watchRecorder) Watch(sources map[snowflake.ID]snowflake.ID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watched = append(w.watched, sources)
}

func (w *watchRecorder) calls() []map[snowflake.ID]snowflake.ID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watched
}

// Every pipeline with a router must hand its capture sources to the session's
// deaf state, or capture bots stay deafened with callers in their channels.
func TestRoutedPipelinesWatchTheirCaptureSources(t *testing.T) {
	host, guest := hostFx(), guestFx()
	channelsOf := func(ownerID, ownerCh snowflake.ID, speakerIDs, speakerChs []snowflake.ID) map[snowflake.ID]snowflake.ID {
		m := map[snowflake.ID]snowflake.ID{ownerID: ownerCh}
		for i, id := range speakerIDs {
			m[id] = speakerChs[i]
		}
		return m
	}
	hostChannels := channelsOf(host.ownerBotID, host.ownerChannelID, host.speakerIDs, host.speakerChIDs)
	guestChannels := channelsOf(guest.ownerBotID, guest.ownerChannelID, guest.speakerIDs, guest.speakerChIDs)
	cases := []struct {
		name      string
		channelOf map[snowflake.ID]snowflake.ID
		build     func(ctx context.Context, deaf DeafSink) error
		wantOwner bool
	}{
		{"host one caller", hostChannels, func(ctx context.Context, deaf DeafSink) error {
			p := buildHostParams(t, ctx, host, guild.RaidModeOneCaller)
			p.Setup.Deaf = deaf
			_, _, err := HostFor(guild.RaidModeOneCaller).Build(ctx, p)
			return err
		}, true},
		{"host guild caller", hostChannels, func(ctx context.Context, deaf DeafSink) error {
			p := buildHostParams(t, ctx, host, guild.RaidModeGuildCaller)
			p.Setup.Deaf = deaf
			_, _, err := HostFor(guild.RaidModeGuildCaller).Build(ctx, p)
			return err
		}, true},
		{"host star caller", hostChannels, func(ctx context.Context, deaf DeafSink) error {
			p := buildHostParams(t, ctx, host, guild.RaidModeOneManyGuildCaller)
			p.Setup.Deaf = deaf
			_, _, err := HostFor(guild.RaidModeOneManyGuildCaller).Build(ctx, p)
			return err
		}, true},
		{"guest caller", guestChannels, func(ctx context.Context, deaf DeafSink) error {
			p := buildGuestParams(t, ctx, guest, guild.RaidModeGuildCaller, guild.RaidModeAllyCaller)
			p.Setup.Deaf = deaf
			_, _, cleanup, err := GuestFor(guild.RaidModeAllyCaller).Build(ctx, p)
			if cleanup != nil {
				cleanup()
			}
			return err
		}, false},
		{"guest star caller", guestChannels, func(ctx context.Context, deaf DeafSink) error {
			p := buildGuestParams(t, ctx, guest, guild.RaidModeGuildCaller, guild.RaidModeOneManyAllyCaller)
			p.Setup.Deaf = deaf
			_, _, cleanup, err := GuestFor(guild.RaidModeOneManyAllyCaller).Build(ctx, p)
			if cleanup != nil {
				cleanup()
			}
			return err
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rec := &watchRecorder{}
			if err := c.build(ctx, rec); err != nil {
				t.Fatalf("build: %v", err)
			}

			calls := rec.calls()
			if len(calls) != 1 {
				t.Fatalf("Watch called %d times, want once", len(calls))
			}
			sources := calls[0]
			if len(sources) == 0 {
				t.Fatal("Watch was handed no capture sources")
			}
			for botID, channelID := range sources {
				want, known := c.channelOf[botID]
				if !known || channelID != want {
					t.Errorf("source %d watched in channel %d, want its own channel %d", botID, channelID, want)
				}
			}
			if _, owner := sources[host.ownerBotID]; c.wantOwner && !owner {
				t.Errorf("owner is not among the watched sources %v", sources)
			}
		})
	}
}
