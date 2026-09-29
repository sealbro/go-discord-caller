package router

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// Copy mode points a source's raw-Opus writer straight at the destination's
// ChOuts, which is safe only while that destination's mixer is paused — the
// RelayFeed doc spells out why the two must never share ChOuts. The mixer runs
// on its own goroutine, so the pause has to be in place before the copy install
// is published, not after: otherwise a tick landing in between puts a
// re-encoded mix and a raw packet into the same speaker channel, and the
// listener decodes two interleaved streams.
func TestMixerIsPausedBeforeCopyInstall(t *testing.T) {
	const (
		guildID  = snowflake.ID(1430511050704289835)
		roleID   = snowflake.ID(1529403555667251310)
		sourceCh = snowflake.ID(2001)
		destCh   = snowflake.ID(2002)
	)

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("pause_order_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	mixer, err := opus.NewMixer(metrics.Opus.For(guildID.String()))
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}

	var pausedAtCopyInstall *bool
	src := &SourceSlot{
		ID:        sourceCh,
		ChannelID: sourceCh,
		Handle:    opus.NewFanoutHandle(),
		BuildInstall: func(mode RouteMode, _ []UserBinding) (opus.FanoutInstall, func()) {
			if mode == RouteCopy {
				p := mixer.Paused()
				pausedAtCopyInstall = &p
			}
			return opus.FanoutInstall{}, func() {}
		},
	}
	dest := &DestSlot{
		ChannelID: destCh,
		Mixer:     mixer,
		Sources:   []*SourceSlot{src},
		ChOuts:    []chan<- []byte{make(chan []byte, opus.AudioChanBuf)},
	}
	src.Feeds = []*DestSlot{dest}

	probe := &callerCount{counts: map[snowflake.ID]int{sourceCh: 2}}
	r := New(guildID, roleID, probe, []*SourceSlot{src}, []*DestSlot{dest})

	// Two callers: the source mixes and the mixer runs.
	r.Recompute()
	if mixer.Paused() {
		t.Fatal("mixer should run while the source is in mix mode")
	}

	// One caller left: the source falls back to copy and writes raw Opus into
	// the destination's ChOuts itself.
	probe.counts[sourceCh] = 1
	r.Recompute()

	if pausedAtCopyInstall == nil {
		t.Fatal("the copy install never ran")
	}
	if !*pausedAtCopyInstall {
		t.Error("the copy install was published while the mixer was still running: a tick in that window writes a mixed frame into the same channel as the raw packets")
	}
}
