package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// Two speaker bots bound to the same voice channel share one mixer, and each
// drains its own VoiceProvider — which returns every frame it has sent to the
// pool. Handing both the same buffer returns it twice, after which the pool
// serves one backing array to two owners at once and the audio they encode into
// it overwrites each other's.
func TestChannelMixerSinkGivesEachSpeakerItsOwnBuffer(t *testing.T) {
	const (
		guildID   = snowflake.ID(1430511050704289835)
		channelID = snowflake.ID(1529403555667251310)
		sourceID  = snowflake.ID(1529403555667251399)
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("channel_mixer_sink_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gm := metrics.ForGuild(ctx, guildID)

	mx, err := opus.NewMixer(gm.Opus)
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}
	src := opus.NewSourceBuffer(nil)
	if err := mx.AddInput(sourceID, src); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	first := make(chan []byte, opus.AudioChanBuf)
	second := make(chan []byte, opus.AudioChanBuf)
	dest := &DestChannel{ChannelID: channelID, Outs: []chan<- []byte{first, second}}

	StartChannelMixers(ctx, gm, []*DestChannel{dest}, map[snowflake.ID]*opus.Mixer{channelID: mx}, nil)

	pcm := opus.GetPCM()
	for i := range pcm {
		pcm[i] = int16(i % 1000)
	}
	src.Feed(opus.Frame{PCM: pcm, Opus: opus.CopyOpusFrame([]byte("opus-frame")), CreatedAt: time.Now()})

	a := receiveFrame(t, first, "first speaker")
	b := receiveFrame(t, second, "second speaker")

	if &a[0] == &b[0] {
		t.Error("both speakers were handed the same buffer; each returns it to the pool after sending, so one array ends up with two owners")
	}
}

func receiveFrame(t *testing.T, ch <-chan []byte, who string) []byte {
	t.Helper()
	select {
	case frame := <-ch:
		if len(frame) == 0 {
			t.Fatalf("%s: got an empty frame", who)
		}
		return frame
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no frame arrived", who)
		return nil
	}
}
