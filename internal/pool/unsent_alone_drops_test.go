package pool

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"

	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

type speakerConn struct {
	voice.Conn
	session godave.Session
	udp     voice.UDPConn
}

func (c *speakerConn) DAVE() godave.Session { return c.session }
func (c *speakerConn) UDP() voice.UDPConn   { return c.udp }
func (c *speakerConn) SetSpeaking(context.Context, voice.SpeakingFlags) error {
	return nil
}

type discardUDP struct{ voice.UDPConn }

func (discardUDP) Write(p []byte) (int, error) { return len(p), nil }

type speakerSession struct {
	godave.Session
	ready  bool
	unsent atomic.Int64
}

func (s *speakerSession) Ready() bool        { return s.ready }
func (s *speakerSession) RecordUnsentFrame() { s.unsent.Add(1) }

// dropsFor runs a real disgo audio sender against a speaker queue fed at 50
// frames per second on average, in back-to-back groups of three as the network
// hands them over, and returns how many frames the producer found the queue full
// for and how many the sender discarded unsent.
func dropsFor(t *testing.T, ready bool) (dropped, unsent int64) {
	t.Helper()

	queue := make(chan []byte, opus.AudioChanBuf)
	provider := opus.NewVoiceProvider(queue, telemetry.OpusRecorder{})
	defer provider.Close()

	session := &speakerSession{ready: ready}
	registry := NewAudioSenderRegistry()
	conn := &speakerConn{session: session, udp: discardUDP{}}
	sender := registry.CreateFunc()(slog.New(slog.NewTextHandler(io.Discard, nil)), provider, conn)
	sender.Open()
	defer sender.Close()

	const (
		burst  = 3
		frames = 750
	)
	frame := make([]byte, 80)
	var full atomic.Int64

	produce := time.NewTicker(burst * 20 * time.Millisecond)
	defer produce.Stop()
	for sent := 0; sent < frames; sent += burst {
		<-produce.C
		for range burst {
			select {
			case queue <- frame:
			default:
				full.Add(1)
			}
		}
	}
	return full.Load(), session.unsent.Load()
}

// TestAloneSpeakerQueueAbsorbsBursts is the regression test for the direct-path
// frame drops production showed on speaker bots that were alone in their channel
// (five bots dropped ~12.6k frames each in one raid while discarding every frame
// they were handed as unsent_alone).
//
// A speaker with a listener absorbs a burst of three frames because the sender
// blocks in ProvideOpusFrame and takes each frame as it arrives. A speaker whose
// DAVE session is not ready discards one frame per 20 ms tick instead, so a burst
// that lands on a queue still holding a frame overflows it. Nobody hears that bot,
// so the overflow is pure alarm noise, and it must not exceed what a listened bot
// shows for the same traffic.
func TestAloneSpeakerQueueAbsorbsBursts(t *testing.T) {
	listenedDrops, _ := dropsFor(t, true)
	aloneDrops, aloneUnsent := dropsFor(t, false)

	if listenedDrops != 0 {
		t.Fatalf("control is wrong: a speaker with a listener dropped %d frames", listenedDrops)
	}
	if aloneDrops != 0 {
		t.Errorf("a speaker alone in its channel dropped %d frames of the same traffic (and discarded %d as unsent); a speaker with a listener dropped none",
			aloneDrops, aloneUnsent)
	}
}
