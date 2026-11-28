package pipeline

import (
	"context"
	"iter"
	"slices"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// recordingVoiceManager is a voice.Manager that reports the Leave attempt and
// hands back no connection, so GuildVoice.Leave is observable without any
// Discord I/O.
type recordingVoiceManager struct {
	note func(string)
}

func (v *recordingVoiceManager) HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate)   {}
func (v *recordingVoiceManager) HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate) {}
func (v *recordingVoiceManager) CreateConn(snowflake.ID) voice.Conn                     { return nil }
func (v *recordingVoiceManager) RemoveConn(snowflake.ID)                                {}
func (v *recordingVoiceManager) Close(context.Context)                                  {}

func (v *recordingVoiceManager) Conns() iter.Seq[voice.Conn] {
	return func(func(voice.Conn) bool) {}
}

func (v *recordingVoiceManager) GetConn(snowflake.ID) voice.Conn {
	v.note("leave")
	return nil
}

// eventLog collects cleanup steps from the per-speaker goroutines
// BuildSpeakerCleanup spawns.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) note(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, s)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// TestBuildSpeakerCleanupClosesBeforeLeave: a speaker's provider and receiver
// are closed before it leaves its channel.
func TestBuildSpeakerCleanupClosesBeforeLeave(t *testing.T) {
	log := &eventLog{}
	joined := []SpeakerResult{{
		Speaker: guild.Speaker{ID: snowflake.ID(1234)},
		GV:      pool.NewGuildVoice(&recordingVoiceManager{note: log.note}, snowflake.ID(5678)),
		Cleanup: func() { log.note("cleanup") },
	}}

	BuildSpeakerCleanup(snowflake.ID(42), joined)()

	want := []string{"cleanup", "leave"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
}
