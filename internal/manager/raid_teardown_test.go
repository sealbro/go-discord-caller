package manager

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

// raidSide is one way a guild comes into a raid: hosting it, or joining
// another guild's as a guest. Their teardown paths are separate code with the
// same obligations.
type raidSide struct {
	name    string
	guildID snowflake.ID
	prepare func(f *callerRoleFixture)
	start   func(f *callerRoleFixture) (context.CancelFunc, error)
}

var raidSides = []raidSide{
	{
		name:    "host",
		guildID: testGuildID,
		prepare: func(*callerRoleFixture) {},
		start: func(f *callerRoleFixture) (context.CancelFunc, error) {
			ctx, cancel := context.WithCancel(context.Background())
			_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
			return cancel, err
		},
	},
	{
		name:    "guest",
		guildID: roleTestGuestGuildID,
		prepare: func(f *callerRoleFixture) {
			f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)
		},
		start: func(f *callerRoleFixture) (context.CancelFunc, error) {
			ctx, cancel := context.WithCancel(context.Background())
			_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)
			return cancel, err
		},
	},
}

func TestRaidStart_OwnerFailureReleasesEverything(t *testing.T) {
	failures := []struct {
		name  string
		apply func(*fakeVoiceConn)
		// joined is whether the owner got into voice and so has to leave.
		joined bool
	}{
		{"owner cannot be set up", func(c *fakeVoiceConn) { c.speakingErr = errors.New("speaking refused") }, true},
		// The speakers join before the owner does, so an owner that cannot
		// join leaves the same things behind as one that joins and then cannot
		// be set up.
		{"owner cannot join", func(c *fakeVoiceConn) { c.openErr = errors.New("join refused") }, false},
	}
	for _, side := range raidSides {
		for _, failure := range failures {
			if side.name == "guest" && !failure.joined {
				continue // a guest tolerates an owner that cannot join
			}
			t.Run(side.name+"/"+failure.name, func(t *testing.T) {
				f := withCallerRole(newCallerRoleFixture(t))
				side.prepare(f)
				failure.apply(f.ownerConn)

				cancel, err := side.start(f)
				defer cancel()

				if err == nil {
					t.Fatal("a raid whose owner bot failed must fail")
				}
				if failure.joined && f.ownerConn.closeCount() == 0 {
					t.Error("owner bot was left in its voice channel after the start failed")
				}
				if f.speakerConn.closeCount() == 0 {
					t.Error("speaker bot was left in its voice channel after the start failed")
				}
				if _, ok := f.m.reconnect.loadApplier(side.guildID, roleTestSpeakerID); ok {
					t.Error("a failed start left a reconnect applier registered")
				}
				if f.m.HasActiveSession(side.guildID) {
					t.Error("a failed start was committed")
				}
				if _, attached := f.m.sessions.GetByGuild(side.guildID); attached {
					t.Error("a failed start stayed registered in an ally session")
				}
			})
		}
	}
}

func blockOwnerLeave(f *callerRoleFixture) (entered <-chan struct{}, release func()) {
	in := make(chan struct{}, 1)
	block := make(chan struct{})
	f.ownerConn.setOnClose(func() {
		select {
		case in <- struct{}{}:
		default:
		}
		<-block
	})
	var once sync.Once
	return in, func() { once.Do(func() { close(block) }) }
}

func TestStopVoiceRaid_NewStartRefusedUntilTeardownEnds(t *testing.T) {
	for _, side := range raidSides {
		t.Run(side.name, func(t *testing.T) {
			f := withCallerRole(newCallerRoleFixture(t))
			side.prepare(f)
			cancel, err := side.start(f)
			if err != nil {
				t.Fatalf("first start: %v", err)
			}
			defer cancel()

			entered, release := blockOwnerLeave(f)
			defer release()
			stopped := make(chan error, 1)
			go func() { stopped <- f.m.StopVoiceRaid(context.Background(), side.guildID) }()
			await(t, entered, "the teardown to reach the owner leave")

			cancel2, err := side.start(f)
			cancel2()
			if !errors.Is(err, ErrSessionExists) {
				t.Errorf("start during teardown: err = %v, want ErrSessionExists (the old teardown would tear the new raid down)", err)
			}

			release()
			if err := await(t, stopped, "the stop to return"); err != nil {
				t.Fatalf("stop: %v", err)
			}
			f.ownerConn.setOnClose(nil)

			if side.guildID != testGuildID {
				if _, attached := f.m.sessions.GetByGuild(side.guildID); attached {
					t.Error("guest still attached to the ally session after its teardown finished")
				}
			}
			cancel3, err := side.start(f)
			defer cancel3()
			if err != nil {
				t.Errorf("start after teardown finished: %v", err)
			}
			_ = f.m.StopVoiceRaid(context.Background(), side.guildID)
		})
	}
}

func TestStartVoiceRaid_AbortedStartLeavesWithLiveContext(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.ownerVM.onOpen = cancel

	_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)

	if err == nil {
		t.Fatal("a start cancelled mid-way must fail")
	}
	errs := f.ownerConn.closeContextErrors()
	if len(errs) == 0 {
		t.Fatal("owner bot never left")
	}
	for _, e := range errs {
		if e != nil {
			t.Errorf("owner leave ran on a cancelled context (%v): disgo drops such a leave and the bot stays in the channel", e)
		}
	}
}

func TestReconnectBotChannel_LeavesWhenSessionEndedDuringJoin(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.m.mu.Lock()
	f.m.statuses[testGuildID].Session = &guild.Session{GuildID: testGuildID}
	f.m.mu.Unlock()
	f.speakerVM.onOpen = func() {
		f.m.mu.Lock()
		f.m.statuses[testGuildID].Session = nil
		f.m.mu.Unlock()
	}

	f.m.ReconnectBotChannel(context.Background(), testGuildID, roleTestSpeakerID)

	if n := f.speakerConn.closeCount(); n < 2 {
		t.Errorf("speaker closed %d time(s): after the rejoin the session was gone, so the bot must leave again instead of sitting in the channel", n)
	}
}

func TestStartVoiceRaid_RoleGrantDuringStartupIsKept(t *testing.T) {
	const userID = snowflake.ID(42)
	f := withCallerRole(newCallerRoleFixture(t))
	putInVoice(f, userID, roleTestChannels[testGuildID][0], false, viewTestOtherRole)
	f.ownerVM.onOpen = func() {
		granted := discord.Member{
			GuildID: testGuildID,
			User:    discord.User{ID: userID},
			RoleIDs: []snowflake.ID{roleTestCallerRoleID},
		}
		f.caches.AddMember(granted) // as the member-update handler does before notifying
		f.m.NotifyMemberUpdate(testGuildID, granted)
	}

	_, cancel, err := startHostRaid(t, f)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cancel()
	defer func() { _ = f.m.StopVoiceRaid(context.Background(), testGuildID) }()

	f.m.mu.RLock()
	filter, _ := f.m.statuses[testGuildID].Session.AllowFilter.(*AllowFilter)
	f.m.mu.RUnlock()
	if filter == nil {
		t.Fatal("session has no allow filter")
	}
	if !filter.Check(userID) {
		t.Error("a caller role granted while the raid was starting was lost: the member is not relayed")
	}
}

func TestStartVoiceRaid_RefusesSpeakerSharingTheOwnerChannel(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.st.BindChannel(testGuildID, roleTestSpeakerID, roleTestChannels[testGuildID][0])

	_, cancel, err := startHostRaid(t, f)
	defer cancel()

	if !errors.Is(err, ErrChannelTaken) {
		t.Errorf("start with a speaker in the owner's channel: err = %v, want ErrChannelTaken", err)
	}
	if n := f.joins.Load(); n != 0 {
		t.Errorf("%d bot(s) joined voice for a raid that must be refused", n)
	}
}

type blockingToggleStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingToggleStore) SetSpeakerEnabled(guildID, speakerID snowflake.ID, enabled bool) {
	s.entered <- struct{}{}
	<-s.release
	s.Store.SetSpeakerEnabled(guildID, speakerID, enabled)
}

func TestToggleSpeaker_StoreWriteDoesNotHoldServiceLock(t *testing.T) {
	f := newCallerRoleFixture(t)
	bs := &blockingToggleStore{Store: f.st, entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.m.store = bs

	done := make(chan error, 1)
	go func() { done <- f.m.ToggleSpeaker(testGuildID, roleTestSpeakerID, false) }()
	await(t, bs.entered, "the toggle to reach the store")

	read := make(chan struct{})
	go func() {
		f.m.HasActiveSession(testGuildID)
		close(read)
	}()
	await(t, read, "a state read during the store write: if the service lock is held across it, every guild's state is blocked behind a disk write")
	close(bs.release)
	if err := <-done; err != nil {
		t.Fatalf("ToggleSpeaker: %v", err)
	}
}

type recordingConn struct {
	fakeVoiceConn
	mu2       sync.Mutex
	providers []voice.OpusFrameProvider
}

func (c *recordingConn) SetOpusFrameProvider(p voice.OpusFrameProvider) {
	c.mu2.Lock()
	c.providers = append(c.providers, p)
	c.mu2.Unlock()
}

func TestBuildApplier_ClosesTheProviderItReplaces(t *testing.T) {
	metrics, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("applier_test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m := &Service{metrics: metrics, reconnect: newReconnectState()}
	chOut := make(chan []byte, 1)
	apply := m.buildApplier(testGuildID, roleTestSpeakerID, chOut, nil, nil)
	conn := &recordingConn{}

	apply(context.Background(), conn)
	apply(context.Background(), conn)

	if len(conn.providers) != 2 {
		t.Fatalf("providers set = %d, want 2", len(conn.providers))
	}
	closed := make(chan struct{})
	go func() {
		_, _ = conn.providers[0].ProvideOpusFrame()
		close(closed)
	}()
	await(t, closed, "the replaced provider to be closed")
	conn.providers[1].(*opus.VoiceProvider).Close()
}

// joinGuestUntilStopped starts a guest join whose bot handshake on conn hangs,
// /stops it once the handshake is reached, and returns what JoinSession said.
func joinGuestUntilStopped(t *testing.T, f *callerRoleFixture, conn *fakeVoiceConn) error {
	t.Helper()
	f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)
	joining := make(chan struct{})
	conn.open = func(ctx context.Context) error {
		close(joining)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan error, 1)
	go func() {
		_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)
		joined <- err
	}()

	await(t, joining, "the join to reach the bot handshake")
	if err := f.m.StopVoiceRaid(context.Background(), roleTestGuestGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}
	return await(t, joined, "the join to return")
}

// The guest tolerates an owner that cannot join, so a /stop during the owner's
// handshake used to fall through to the commit: the session came up only to
// tear itself down, and /start reported the guest as joined.
func TestJoinSession_StoppedDuringOwnerJoinFailsAndCommitsNothing(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))

	err := joinGuestUntilStopped(t, f, f.ownerConn)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("join stopped during the owner handshake returned %v, want it to wrap context.Canceled", err)
	}
	if f.m.HasActiveSession(roleTestGuestGuildID) {
		t.Error("a stopped join was committed")
	}
	if _, attached := f.m.sessions.GetByGuild(roleTestGuestGuildID); attached {
		t.Error("a stopped join stayed attached to the host's ally session")
	}
}

func TestJoinSession_StoppedDuringSpeakerJoinsReportsCancellation(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))

	err := joinGuestUntilStopped(t, f, f.speakerConn)

	if errors.Is(err, ErrNoSpeakers) {
		t.Errorf("a join stopped during the speaker joins reported %q", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("join error = %v, want it to wrap context.Canceled", err)
	}
}
