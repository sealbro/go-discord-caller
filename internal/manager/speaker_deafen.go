package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Server-deafening speaker bots in non-capture raid modes.
//
// A speaker bot in a non-capture mode (RaidMode.WithCapture() == false) never
// consumes inbound audio: VoiceConnSetup.Apply gives it an
// opus.EmptyVoiceReceiver that discards every frame. Discord does not care —
// the voice server keeps forwarding RTP for every speaking member of the
// channel, and disgo's udpConnImpl.ReadPacket transport-decrypts AND
// DAVE-decrypts each packet *before* the receiver is consulted. That is 50
// wasted decrypts per second per speaking human, per speaker bot, and it
// scales with how busy the destination channel is.
//
// Measured against a live guild on 2026-09-09, one source bot playing
// continuously into a channel with one listener bot:
//
//	self_deaf flipped on a live connection   50.0/s -> 50.0/s  (no effect)
//	self_deaf set at handshake (conn.Open)   50.0/s            (no effect)
//	server deafen (PATCH guild member)       50.0/s -> 0.0/s
//	server deafen, measured on the SENDER    50.0/s -> 50.0/s  (still sends)
//
// So self_deaf is a client-side flag the voice server ignores for forwarding,
// and only server deafen actually stops the stream. Crucially it is
// receive-only: a server-deafened bot keeps sending at full rate, which is
// what makes it usable for speaker bots at all.
//
// The catch is permissions. Server deafen is a moderation action, so the owner
// bot needs DEAFEN_MEMBERS (bit 23) in the guild AND a role above every speaker
// bot's role; without both, Discord answers 50013 Missing Permissions. Guilds
// that installed the bot before that permission was added to the invite link
// simply do not have it, so this must degrade quietly rather than fail a raid —
// see reconcileSpeakerDeaf.
//
// Two further consequences drive the design below:
//
//   - The flag persists on the guild member across voice sessions, and a
//     deafened bot cannot capture. A crash mid-session would therefore leave a
//     speaker silently unable to capture in the *next* raid. Every join
//     reconciles the flag to what the mode requires rather than assuming a
//     clean slate — that reconcile is a correctness requirement, not a tidiness
//     one.
//   - It is a moderation action: each real change costs a REST call and a guild
//     audit-log entry. ensureSpeakerDeaf skips the call when the cached voice
//     state already agrees, which keeps the common capture-mode reconcile free.

// ensureSpeakerDeaf drives speakerID's server-deaf flag in guildID to want.
//
// The cached voice state is authoritative for the server-deaf bit and is
// refreshed by the VOICE_STATE_UPDATE that the speaker's own join produces, so
// a hit is trustworthy. A miss falls through to the REST call rather than
// assuming anything — being wrong in the "already undeafened" direction would
// silently break capture.
func (m *Service) ensureSpeakerDeaf(ctx context.Context, guildID, speakerID snowflake.ID, want bool) error {
	if vs, ok := m.ownerClient.Caches.VoiceState(guildID, speakerID); ok && vs.GuildDeaf == want {
		return nil
	}
	if _, err := m.ownerClient.Rest.UpdateMember(guildID, speakerID,
		discord.MemberUpdate{Deaf: &want}, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("set server deaf=%t for speaker %s: %w", want, speakerID, err)
	}
	return nil
}

// reconcileSpeakerDeaf brings a freshly joined speaker's server-deaf flag in
// line with what the raid mode needs, and returns the undoing func to hang on
// the SpeakerResult (nil when there is nothing to undo).
//
// Non-capture modes deafen: the bot discards inbound audio anyway, so this
// just stops Discord sending it. Capture modes undeafen, which is what repairs
// a flag stranded by a previous non-capture session that never tore down
// cleanly — without it a speaker would join, look healthy, and capture silence.
//
// Failures are logged and swallowed. Deafening is an optimisation, and a raid
// that refuses to start because a PATCH was rate-limited would trade a large
// win for a much worse failure mode. The reconcile direction is the one that
// matters, and a capture-mode failure there surfaces as the existing
// "no audio from speaker" symptom rather than something new.
func (m *Service) reconcileSpeakerDeaf(ctx context.Context, guildID, speakerID snowflake.ID, withCapture bool, deaf *deafController) func(context.Context) {
	if withCapture {
		if err := m.ensureSpeakerDeaf(ctx, guildID, speakerID, false); err != nil {
			deaf.noteError(err)
			slog.WarnContext(ctx, "failed to clear speaker server-deaf; capture may be silent",
				slog.String("guildID", guildID.String()),
				slog.String("speakerID", speakerID.String()),
				slog.Any("err", err))
		}
		return nil
	}

	if err := m.ensureSpeakerDeaf(ctx, guildID, speakerID, true); err != nil {
		deaf.noteError(err)
		slog.WarnContext(ctx, "failed to server-deafen speaker; it will keep decrypting unused audio",
			slog.String("guildID", guildID.String()),
			slog.String("speakerID", speakerID.String()),
			slog.Any("err", err))
		return nil
	}

	return func(cleanupCtx context.Context) {
		if err := m.ensureSpeakerDeaf(cleanupCtx, guildID, speakerID, false); err != nil {
			slog.WarnContext(cleanupCtx, "failed to undeafen speaker on teardown; next capture raid will repair it",
				slog.String("guildID", guildID.String()),
				slog.String("speakerID", speakerID.String()),
				slog.Any("err", err))
		}
	}
}

// deafenDelay is how long a bot must look idle before it is actually deafened.
//
// The asymmetry is deliberate: undeafening happens immediately, deafening
// waits. Getting undeafening wrong costs audio — the bot misses the first
// words of whoever just gained the role — while getting deafening wrong costs
// only a few seconds of decrypting frames nobody reads. The delay also
// collapses flapping: a caller who leaves and rejoins, or an admin toggling a
// role, produces no deaf transition at all, and therefore no audit-log entry.
const deafenDelay = 5 * time.Second

// deafController owns the server-deaf flag of every bot in one session for as
// long as that session lives.
//
// The router reports which capture sources are live after each recomputation
// (router.WithCaptureObserver). A bot should hear Discord exactly while it is
// a live capture source; everything else — non-capture modes, and the second
// and later speakers sharing a channel, whose FanoutHandles are never
// installed (see pipeline.IterDeduplicatedCaptures) — is decrypting audio it
// discards, and gets deafened.
//
// Every registered bot is driven, not just the ones the router knows about: a
// bot absent from the capturing map is by definition not a live source.
type deafController struct {
	guildID snowflake.ID
	// setDeaf performs the actual change. Injected so the controller's timing
	// rules can be unit-tested without a Discord client.
	setDeaf func(ctx context.Context, guildID, botID snowflake.ID, deaf bool) error
	// delay is how long a bot must look idle before being deafened; overridden
	// in tests to keep them fast.
	delay time.Duration

	mu      sync.Mutex
	members map[snowflake.ID]*deafMember
	closed  bool
	// disabled latches when Discord refuses a change for lack of permission.
	// That is a static property of the guild, not a transient failure, so
	// retrying every deafenDelay would spend the rest of the session issuing
	// 403s — which Discord's edge treats as abuse. One refusal is enough.
	disabled bool
}

type deafMember struct {
	deaf bool // last state Discord confirmed for this member
	// inFlight is set while a change is being applied. Without it, an Observe
	// arriving between "timer fired" and "Discord accepted" would see deaf
	// still false, arm a second timer, and issue a duplicate PATCH.
	inFlight bool
	timer    *time.Timer // pending delayed deafen; nil when none scheduled
}

func newDeafController(m *Service, guildID snowflake.ID) *deafController {
	return &deafController{
		guildID: guildID,
		setDeaf: m.ensureSpeakerDeaf,
		delay:   deafenDelay,
		members: map[snowflake.ID]*deafMember{},
	}
}

// Register enrols a bot with the state reconcileSpeakerDeaf already applied at
// join, so the controller does not re-issue a PATCH for a value it is already at.
func (c *deafController) Register(botID snowflake.ID, deaf bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.members[botID] = &deafMember{deaf: deaf}
}

// Observe is the router.WithCaptureObserver callback.
func (c *deafController) Observe(capturing map[snowflake.ID]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed || c.disabled {
		c.mu.Unlock()
		return
	}
	type action struct {
		botID snowflake.ID
		deaf  bool
	}
	var now []action
	for botID, mem := range c.members {
		if mem.inFlight {
			continue
		}
		want := !capturing[botID]
		if want == mem.deaf {
			// Already correct. Cancel a pending move in the other direction.
			if mem.timer != nil {
				mem.timer.Stop()
				mem.timer = nil
			}
			continue
		}
		if !want {
			// Undeafen: immediately, and drop any pending deafen.
			if mem.timer != nil {
				mem.timer.Stop()
				mem.timer = nil
			}
			mem.inFlight = true
			now = append(now, action{botID, false})
			continue
		}
		// Deafen: only after the bot has stayed idle for deafenDelay.
		if mem.timer != nil {
			continue // already counting down
		}
		id := botID
		mem.timer = time.AfterFunc(c.delay, func() { c.deafenNow(id) })
	}
	c.mu.Unlock()

	for _, a := range now {
		c.apply(a.botID, a.deaf)
	}
}

// deafenNow fires when a bot has been idle for deafenDelay without the router
// reporting it live again.
func (c *deafController) deafenNow(botID snowflake.ID) {
	c.mu.Lock()
	mem, ok := c.members[botID]
	if !ok || c.closed || c.disabled || mem.timer == nil {
		c.mu.Unlock()
		return
	}
	mem.timer = nil
	mem.inFlight = true
	c.mu.Unlock()
	c.apply(botID, true)
}

// apply issues the REST change off the caller's goroutine. The router invokes
// Observe from its debounce timer, and a PATCH must never stall routing.
//
// The member's recorded state is updated only after Discord accepts the change.
// Recording it optimistically would make Close try to undo a deafen that never
// happened, and would let a failed deafen masquerade as applied.
func (c *deafController) apply(botID snowflake.ID, deaf bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deafApplyTimeout)
		defer cancel()
		err := c.setDeaf(ctx, c.guildID, botID, deaf)
		c.mu.Lock()
		if mem, ok := c.members[botID]; ok {
			mem.inFlight = false
			if err == nil {
				mem.deaf = deaf
			}
		}
		c.mu.Unlock()
		if err != nil {
			c.noteError(err)
			slog.WarnContext(ctx, "failed to apply dynamic server-deaf",
				slog.String("guildID", c.guildID.String()),
				slog.String("botID", botID.String()),
				slog.Bool("deaf", deaf),
				slog.Any("err", err))
		}
	}()
}

// deafApplyTimeout caps a single deaf PATCH, including disgo's rate-limit wait.
const deafApplyTimeout = 10 * time.Second

// jsonErrMissingPermissions is Discord's error code for a request the bot is
// not allowed to make. Returned when the owner bot lacks DEAFEN_MEMBERS or
// does not outrank the target.
const jsonErrMissingPermissions rest.JSONErrorCode = 50013

// isPermissionError reports whether err is Discord refusing on authorisation
// grounds, as opposed to a transient failure worth retrying.
func isPermissionError(err error) bool {
	var restErr *rest.Error
	if !errors.As(err, &restErr) {
		return false
	}
	if restErr.Code == jsonErrMissingPermissions {
		return true
	}
	return restErr.Response != nil && restErr.Response.StatusCode == http.StatusForbidden
}

// noteError latches the controller off when Discord says the bot may not do
// this at all. Called from both the join-time reconcile and the dynamic path.
func (c *deafController) noteError(err error) {
	if c == nil || !isPermissionError(err) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return
	}
	c.disabled = true
	for _, mem := range c.members {
		if mem.timer != nil {
			mem.timer.Stop()
			mem.timer = nil
		}
	}
	slog.Warn("server-deafen disabled for this session: the owner bot lacks DEAFEN_MEMBERS "+
		"or does not outrank the speakers; grant it and place its role above them to enable the optimisation",
		slog.String("guildID", c.guildID.String()))
}

// Close stops all pending deafens and undeafens every bot this controller left
// deafened, so a session never strands the flag on a member. Safe to call more
// than once; the per-speaker Undeafen in BuildSpeakerCleanup covers sessions
// that have no router at all (RaidModeAllyListener).
func (c *deafController) Close(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	var restore []snowflake.ID
	for botID, mem := range c.members {
		if mem.timer != nil {
			mem.timer.Stop()
			mem.timer = nil
		}
		if mem.deaf {
			restore = append(restore, botID)
		}
	}
	c.mu.Unlock()

	for _, botID := range restore {
		if err := c.setDeaf(ctx, c.guildID, botID, false); err != nil {
			slog.WarnContext(ctx, "failed to undeafen bot on session close; next capture raid will repair it",
				slog.String("guildID", c.guildID.String()),
				slog.String("botID", botID.String()),
				slog.Any("err", err))
		}
	}
}

// isDisabled reports whether the permission latch has tripped. Test-only.
func (c *deafController) isDisabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disabled
}
