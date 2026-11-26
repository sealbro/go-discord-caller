package pipeline

import (
	"context"
	"iter"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// recordingVoiceManager is a voice.Manager that reports the Leave attempt and
// hands back conn (nil unless set), so GuildVoice.Leave is observable without
// any Discord I/O.
type recordingVoiceManager struct {
	note func(string)
	conn voice.Conn
}

// leaveCtxConn records the context error its Close is called with: a leave
// sent on an expired context never reaches Discord.
type leaveCtxConn struct {
	voice.Conn
	closeErr chan error
}

func (c *leaveCtxConn) Close(ctx context.Context) { c.closeErr <- ctx.Err() }

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
	return v.conn
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

// A stalled undeafen must not spend the time the speaker needs to leave: a
// leave sent on an expired context never reaches Discord, so the bot stays
// in the channel after the raid.
func TestBuildSpeakerCleanupLeavesAfterAStalledUndeafen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := &leaveCtxConn{closeErr: make(chan error, 1)}
		joined := []SpeakerResult{{
			Speaker: guild.Speaker{ID: snowflake.ID(1234)},
			GV:      pool.NewGuildVoice(&recordingVoiceManager{note: func(string) {}, conn: conn}, snowflake.ID(5678)),
			Undeafen: func(ctx context.Context) {
				select {
				case <-ctx.Done():
				case <-time.After(time.Hour):
					t.Error("stalled undeafen was never given a deadline")
				}
			},
		}}

		BuildSpeakerCleanup(snowflake.ID(42), joined)()

		if err := <-conn.closeErr; err != nil {
			t.Errorf("leave was sent on a context already %v: the stalled undeafen spent the leave's budget", err)
		}
	})
}

// memberBucket stands in for disgo's per-guild member-update rate limit:
// requests take it one at a time, waiting under their own deadline, and the
// first one is never answered until its deadline ends.
type memberBucket struct {
	lock       chan struct{}
	mu         sync.Mutex
	calls      int
	undeafened []snowflake.ID
}

func (b *memberBucket) undeafen(id snowflake.ID) func(context.Context) {
	return func(ctx context.Context) {
		select {
		case b.lock <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-b.lock }()

		b.mu.Lock()
		b.calls++
		first := b.calls == 1
		b.mu.Unlock()
		if first {
			<-ctx.Done()
			return
		}
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		b.undeafened = append(b.undeafened, id)
		b.mu.Unlock()
	}
}

// Speakers' undeafens queue on one rate limit, so a deadline that starts
// before a request is sent is spent waiting behind a stalled one.
func TestBuildSpeakerCleanupUndeafensEverySpeakerPastAStalledRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := &memberBucket{lock: make(chan struct{}, 1)}
		var joined []SpeakerResult
		for _, id := range []snowflake.ID{1301, 1302, 1303} {
			joined = append(joined, SpeakerResult{
				Speaker:  guild.Speaker{ID: id},
				GV:       pool.NewGuildVoice(&recordingVoiceManager{note: func(string) {}}, id),
				Undeafen: bucket.undeafen(id),
			})
		}

		BuildSpeakerCleanup(snowflake.ID(42), joined)()

		if got, want := len(bucket.undeafened), len(joined)-1; got != want {
			t.Errorf("%d of %d speakers undeafened behind one stalled request (undeafened %v); "+
				"each undeafen's deadline must start when it is sent", got, want, bucket.undeafened)
		}
	})
}

// Even when Discord answers no undeafen at all, teardown must end within its
// budget: the guild cannot start a new raid until it does.
func TestBuildSpeakerCleanupUndeafensWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var joined []SpeakerResult
		for id := snowflake.ID(1311); id <= 1323; id++ {
			joined = append(joined, SpeakerResult{
				Speaker:  guild.Speaker{ID: id},
				GV:       pool.NewGuildVoice(&recordingVoiceManager{note: func(string) {}}, id),
				Undeafen: func(ctx context.Context) { <-ctx.Done() },
			})
		}

		start := time.Now()
		BuildSpeakerCleanup(snowflake.ID(42), joined)()

		if took := time.Since(start); took > TeardownUndeafenBudget {
			t.Errorf("teardown of %d stalled undeafens took %v, want at most %v", len(joined), took, TeardownUndeafenBudget)
		}
	})
}
