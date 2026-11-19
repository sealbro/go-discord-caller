package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/store"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
)

func withCallerRole(f *callerRoleFixture) *callerRoleFixture {
	f.st.BindRole(testGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	f.st.BindRole(roleTestGuestGuildID, store.RoleTypeCaller, roleTestCallerRoleID)
	return f
}

func startHostRaid(t *testing.T, f *callerRoleFixture) (context.Context, context.CancelFunc, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := f.m.StartVoiceRaid(ctx, testGuildID, cancel, guild.RaidModeOneCaller)
	return ctx, cancel, err
}

func TestStartVoiceRaid_OwnerSetupFailureReleasesEverything(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.ownerConn.speakingErr = errors.New("speaking refused")

	_, cancel, err := startHostRaid(t, f)
	defer cancel()

	if err == nil {
		t.Fatal("a raid whose owner bot could not be set up must fail")
	}
	if f.ownerConn.closeCount() == 0 {
		t.Error("owner bot was left in its voice channel after the start failed")
	}
	if f.speakerConn.closeCount() == 0 {
		t.Error("speaker bot was left in its voice channel after the start failed")
	}
	if _, ok := f.m.reconnect.loadApplier(testGuildID, roleTestSpeakerID); ok {
		t.Error("a failed start left a reconnect applier registered")
	}
	if f.m.HasActiveSession(testGuildID) {
		t.Error("a failed start was committed")
	}
}

// The speakers join before the owner does, so an owner that cannot join leaves
// the same things behind as one that joins and then cannot be set up.
func TestStartVoiceRaid_OwnerJoinFailureReleasesEverything(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.ownerConn.openErr = errors.New("join refused")

	_, cancel, err := startHostRaid(t, f)
	defer cancel()

	if err == nil {
		t.Fatal("a raid whose owner bot could not join must fail")
	}
	if f.speakerConn.closeCount() == 0 {
		t.Error("speaker bot was left in its voice channel after the start failed")
	}
	if _, ok := f.m.reconnect.loadApplier(testGuildID, roleTestSpeakerID); ok {
		t.Error("a failed start left a reconnect applier registered")
	}
	if f.m.HasActiveSession(testGuildID) {
		t.Error("a failed start was committed")
	}
}

func TestJoinSession_OwnerSetupFailureLeavesOwnerAndFails(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)
	f.ownerConn.speakingErr = errors.New("speaking refused")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)

	if err == nil {
		t.Fatal("a guest whose owner bot could not be set up must fail to join")
	}
	if f.ownerConn.closeCount() == 0 {
		t.Error("owner bot was left in its voice channel after the join failed")
	}
	if f.m.HasActiveSession(roleTestGuestGuildID) {
		t.Error("a failed join was committed")
	}
	if _, attached := f.m.sessions.GetByGuild(roleTestGuestGuildID); attached {
		t.Error("a failed join stayed attached to the host's ally session")
	}
	if _, ok := f.m.reconnect.loadApplier(roleTestGuestGuildID, roleTestSpeakerID); ok {
		t.Error("a failed join left a reconnect applier registered")
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

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestStopVoiceRaid_NewStartRefusedUntilTeardownEnds(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	_, cancel, err := startHostRaid(t, f)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer cancel()

	entered, release := blockOwnerLeave(f)
	defer release()
	stopped := make(chan error, 1)
	go func() { stopped <- f.m.StopVoiceRaid(context.Background(), testGuildID) }()
	awaitSignal(t, entered, "the teardown to reach the owner leave")

	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	_, err = f.m.StartVoiceRaid(ctx, testGuildID, cancel2, guild.RaidModeOneCaller)
	if !errors.Is(err, ErrSessionExists) {
		t.Errorf("start during teardown: err = %v, want ErrSessionExists (the old teardown would tear the new raid down)", err)
	}

	release()
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.ownerConn.setOnClose(nil)

	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	if _, err := f.m.StartVoiceRaid(ctx3, testGuildID, cancel3, guild.RaidModeOneCaller); err != nil {
		t.Errorf("start after teardown finished: %v", err)
	}
	_ = f.m.StopVoiceRaid(context.Background(), testGuildID)
}

func TestStopVoiceRaid_GuestNewJoinRefusedUntilTeardownEnds(t *testing.T) {
	f := withCallerRole(newCallerRoleFixture(t))
	f.m.sessions.Create(roleTestHostCode, testGuildID, guild.RaidModeGuildCaller)

	join := func() (context.CancelFunc, error) {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := f.m.JoinSession(ctx, roleTestGuestGuildID, cancel, guild.RaidModeAllyListener, roleTestHostCode)
		return cancel, err
	}
	cancel, err := join()
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	defer cancel()

	entered, release := blockOwnerLeave(f)
	defer release()
	stopped := make(chan error, 1)
	go func() { stopped <- f.m.StopVoiceRaid(context.Background(), roleTestGuestGuildID) }()
	awaitSignal(t, entered, "the teardown to reach the owner leave")

	cancel2, err := join()
	cancel2()
	if !errors.Is(err, ErrSessionExists) {
		t.Errorf("join during teardown: err = %v, want ErrSessionExists", err)
	}

	release()
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.ownerConn.setOnClose(nil)

	if _, attached := f.m.sessions.GetByGuild(roleTestGuestGuildID); attached {
		t.Error("guest still attached to the ally session after its teardown finished")
	}
	cancel3, err := join()
	defer cancel3()
	if err != nil {
		t.Errorf("join after teardown finished: %v", err)
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
	f.ownerVM.onOpen = func() {
		f.m.NotifyMemberUpdate(testGuildID, discord.Member{
			GuildID: testGuildID,
			User:    discord.User{ID: userID},
			RoleIDs: []snowflake.ID{roleTestCallerRoleID},
		})
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
	awaitSignal(t, bs.entered, "the toggle to reach the store")

	read := make(chan struct{})
	go func() {
		f.m.HasActiveSession(testGuildID)
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Error("the service lock is held across the store write: every guild's state is blocked behind a disk write")
	}
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
	select {
	case <-closed:
	case <-time.After(300 * time.Millisecond):
		t.Error("the replaced provider was never closed")
	}
	conn.providers[1].(*opus.VoiceProvider).Close()
}

type lateApplyRecorder struct {
	closedAt atomic.Bool
	late     atomic.Int32
}

func (r *lateApplyRecorder) set(context.Context, snowflake.ID, snowflake.ID, bool) error {
	if r.closedAt.Load() {
		r.late.Add(1)
	}
	return nil
}

func TestDeafController_CloseWaitsForChangeBeingStarted(t *testing.T) {
	rec := &lateApplyRecorder{}
	for range 3000 {
		rec.closedAt.Store(false)
		c := &deafController{guildID: testGuild, setDeaf: rec.set, delay: time.Hour, members: map[snowflake.ID]*deafMember{}}
		c.Register(testBot, true)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Observe(map[snowflake.ID]bool{testBot: true})
		}()
		go func() {
			defer wg.Done()
			c.Close(context.Background())
			rec.closedAt.Store(true)
		}()
		wg.Wait()
	}
	time.Sleep(50 * time.Millisecond)
	if n := rec.late.Load(); n != 0 {
		t.Errorf("%d deaf change(s) started after Close returned: Close did not wait for a change it had already admitted", n)
	}
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

	select {
	case <-joining:
	case <-time.After(5 * time.Second):
		t.Fatal("join never reached the bot handshake")
	}
	if err := f.m.StopVoiceRaid(context.Background(), roleTestGuestGuildID); err != nil {
		t.Fatalf("StopVoiceRaid: %v", err)
	}

	select {
	case err := <-joined:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("join never returned")
		return nil
	}
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
