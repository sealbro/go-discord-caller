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

// TestBuildSpeakerCleanupUndeafensBeforeLeave pins the ordering constraint that
// makes the non-capture server-deafen safe to undo.
//
// Discord rejects a member voice-state PATCH once the member is no longer
// connected to voice, so an Undeafen that ran after GuildVoice.Leave would fail
// and strand the deaf flag on the member record. A stranded flag is not
// cosmetic: a server-deafened bot receives no RTP at all, so the next
// capture-mode raid would join that speaker, look healthy, and capture nothing
// until reconcileSpeakerDeaf repaired it.
func TestBuildSpeakerCleanupUndeafensBeforeLeave(t *testing.T) {
	log := &eventLog{}
	joined := []SpeakerResult{{
		Speaker:  guild.Speaker{ID: snowflake.ID(1234)},
		GV:       pool.NewGuildVoice(&recordingVoiceManager{note: log.note}, snowflake.ID(5678)),
		Cleanup:  func() { log.note("cleanup") },
		Undeafen: func(context.Context) { log.note("undeafen") },
	}}

	BuildSpeakerCleanup(snowflake.ID(42), joined)()

	want := []string{"cleanup", "undeafen", "leave"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
}

// TestBuildSpeakerCleanupSkipsNilUndeafen covers capture modes, where the
// speaker is never deafened and reconcileSpeakerDeaf returns a nil undo func.
func TestBuildSpeakerCleanupSkipsNilUndeafen(t *testing.T) {
	log := &eventLog{}
	joined := []SpeakerResult{{
		Speaker:  guild.Speaker{ID: snowflake.ID(1234)},
		GV:       pool.NewGuildVoice(&recordingVoiceManager{note: log.note}, snowflake.ID(5678)),
		Cleanup:  func() { log.note("cleanup") },
		Undeafen: nil,
	}}

	BuildSpeakerCleanup(snowflake.ID(42), joined)()

	want := []string{"cleanup", "leave"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
}
