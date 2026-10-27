package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

const (
	dropsGuildID   = snowflake.ID(1564203449351020574)
	dropsChannelID = snowflake.ID(1529403555667251310)
	dropsSourceID  = snowflake.ID(1529403555667251399)
	drainingBot    = snowflake.ID(1484911902801924237)
	stuckBot       = snowflake.ID(1484918389884452904)
)

func dropMetrics(t *testing.T) (telemetry.GuildMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("speaker_drops_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return metrics.ForGuild(context.Background(), dropsGuildID), reader
}

// dropsByBot returns gdc.fanout.frames.dropped.total for path, keyed by the
// bot_id label ("" when the drop carries none).
func dropsByBot(t *testing.T, reader *sdkmetric.ManualReader, path telemetry.DropPath) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "gdc.fanout.frames.dropped.total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gdc.fanout.frames.dropped.total: unexpected data type %T", md.Data)
			}
			for _, dp := range sum.DataPoints {
				if p, _ := dp.Attributes.Value("path"); p.AsString() != string(path) {
					continue
				}
				bot, _ := dp.Attributes.Value(attribute.Key("bot_id"))
				got[bot.AsString()] += dp.Value
			}
		}
	}
	return got
}

// A raid that drops frames on the copy path loses some speaker's audio, but a
// guild-wide count cannot say whose: one stuck bot and every bot falling
// behind look the same. Each drop must name the bot whose queue was full.
func TestDirectDropsAreAttributedToTheSpeakerBot(t *testing.T) {
	gm, reader := dropMetrics(t)

	draining := make(chan []byte, opus.AudioChanBuf)
	stuck := make(chan []byte) // nobody reads it, so every send is dropped
	bots := OutputBots{draining: drainingBot, stuck: stuckBot}

	slot := &router.SourceSlot{ID: dropsSourceID, ChannelID: dropsChannelID}
	slot.Feeds = []*router.DestSlot{{ChannelID: dropsChannelID + 1, ChOuts: []chan<- []byte{draining, stuck}}}
	install, _ := routerInstallBuilder(installBuildOpts{
		src:           slot,
		dropDirect:    bots.Dropper(gm, telemetry.DropPathDirect),
		allyBroadcast: opus.PutEncodedFrame,
	})(router.RouteCopy, nil)

	handle := opus.NewFanoutHandle()
	handle.Install(install)
	receiver := opus.NewVoiceReceiver(0, nil, gm.Opus, handle)
	for range 3 {
		if err := receiver.ReceiveOpusFrame(7, &voice.Packet{Opus: []byte{0xFC, 1, 2}}); err != nil {
			t.Fatalf("ReceiveOpusFrame: %v", err)
		}
	}

	got := dropsByBot(t, reader, telemetry.DropPathDirect)
	if got[stuckBot.String()] != 3 || len(got) != 1 {
		t.Errorf("direct drops by bot = %v, want all 3 on the stuck bot %s", got, stuckBot)
	}
}

func TestChannelMixerDropsAreAttributedToTheSpeakerBot(t *testing.T) {
	gm, reader := dropMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mx, err := opus.NewMixer(gm.Opus)
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	src := opus.NewSourceBuffer(nil)
	if err := mx.AddInput(dropsSourceID, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	// The stuck queue comes first, so its drop is recorded before the
	// draining speaker is handed the frame this test waits for.
	stuck := make(chan []byte)
	draining := make(chan []byte, opus.AudioChanBuf)
	dest := &DestChannel{ChannelID: dropsChannelID, Outs: []chan<- []byte{stuck, draining}}
	bots := OutputBots{draining: drainingBot, stuck: stuckBot}

	StartChannelMixers(ctx, gm, []*DestChannel{dest}, map[snowflake.ID]*opus.Mixer{dropsChannelID: mx}, nil, bots)

	pcm := opus.GetPCM()
	for i := range pcm {
		pcm[i] = int16(i % 1000)
	}
	src.Feed(opus.Frame{PCM: pcm, Opus: opus.CopyOpusFrame([]byte("opus-frame")), CreatedAt: time.Now()})
	receiveFrame(t, draining, "draining speaker")

	got := dropsByBot(t, reader, telemetry.DropPathChannelMixer)
	if got[stuckBot.String()] != 1 || len(got) != 1 {
		t.Errorf("channel mixer drops by bot = %v, want 1 on the stuck bot %s", got, stuckBot)
	}
}
