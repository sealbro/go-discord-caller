// Package router is the per-session auto-routing engine. It decides whether
// each captured source runs in off / copy / mix mode based on per-channel
// caller counts (the cascade rule, see docs/AUTO_PIPELINE_PLAN.md §1) and
// owns the SetPaused state for every destination mixer (cascade ∧ listener
// presence).
//
// The package exposes a small public surface that pipelines configure: a
// Router constructor, the SourceSlot / DestSlot topology structs (with
// pipeline-writable fields and router-managed state kept private), and the
// VoiceProbe interfaces (CallerEnumerator + ListenerChecker). Everything
// else is internal.
package router

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// defaultRouteDebounce coalesces bursts of voice events (join/leave/role-update)
// inside this window before triggering a router recompute. 250 ms ≈ 12 Opus
// frames — below the threshold at which a listener can distinguish "the audio
// started flowing through them" from normal voice-connection setup latency.
const defaultRouteDebounce = 250 * time.Millisecond

// RouteMode is the per-source dispatch decision made by the router.
type RouteMode uint8

const (
	// RouteOff: no role-bearing callers are present in the source channel.
	// FanoutHandle installed with empty targets; received frames are dropped.
	RouteOff RouteMode = iota
	// RouteCopy: exactly one caller; frames flow through OpusTargets without
	// decode. Mixer is paused; one source writes directly to chOuts.
	RouteCopy
	// RouteMix: two or more callers, OR a per-destination cascade has forced
	// the source into mix mode because a peer destination requires it.
	// Frames are decoded and fed into the destination mixers' SourceBuffers.
	RouteMix
)

// String renders RouteMode for slog and test failures.
func (m RouteMode) String() string {
	switch m {
	case RouteOff:
		return "off"
	case RouteCopy:
		return "copy"
	case RouteMix:
		return "mix"
	default:
		return "unknown"
	}
}

// CallerEnumerator returns role-bearing users currently in a source channel.
// Feeds the cascade rule (via len()) and the buildInstall closures so each
// user gets their own SourceBuffer per destination mixer (§4.3 fix).
//
// Implementations must be safe to call concurrently and must not retain the
// returned slice. The router never holds its own mu across this call.
type CallerEnumerator interface {
	EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID
}

// ListenerChecker reports whether a destination channel currently has a
// non-bot member who would hear the mixer's output. The router pauses a
// destination's mixer when either the cascade has nothing to mix OR
// HasListeners is false.
//
// Synthetic destinations (relay mixer; ChOuts == nil) skip this check —
// their consumers are ally guests, not local voice-channel humans. The
// router does not call HasListeners for those.
//
// Implementations must be safe to call concurrently.
type ListenerChecker interface {
	HasListeners(channelID snowflake.ID) bool
}

// VoiceProbe is the combined interface the auto-router consumes. Embeds
// both halves so callers that only need one (e.g. a future read-only
// observability tool) can depend on the narrower interface.
type VoiceProbe interface {
	CallerEnumerator
	ListenerChecker
}

// UserBinding pairs a userID with the synthetic mixer-input ID the router
// allocates for that user against a given source. Synth IDs let two different
// users in the same source channel each have their own SourceBuffer in every
// destination mixer (the §4.3 fix), without colliding with real Discord
// snowflakes (synth IDs have bit 63 set; Discord snowflakes don't).
type UserBinding struct {
	UserID  snowflake.ID
	SynthID snowflake.ID
}

// BuildInstallFunc is the pipeline-supplied closure invoked by the router on
// every mode transition (or mix-mode user-set change). Returns the
// FanoutInstall to apply plus a teardown closure that releases any mixer
// inputs or SourceBuffer allocations the install made. The users slice is
// only populated for RouteMix; copy/off modes ignore it.
type BuildInstallFunc func(mode RouteMode, users []UserBinding) (opus.FanoutInstall, func())

// SourceSlot is the per-source topology entry the router operates on.
//
// Pipeline-writable fields (set during pipeline build, never mutated by the
// router): ID, ChannelID, Handle, Feeds, BuildInstall. The router only
// reads them.
//
// Router-managed state (lowercase, accessed only inside this package):
// activeMode, activeUsers, activeTeardown, synthIDs.
type SourceSlot struct {
	ID           snowflake.ID
	ChannelID    snowflake.ID
	Handle       *opus.FanoutHandle
	Feeds        []*DestSlot
	BuildInstall BuildInstallFunc

	// router-managed state
	activeMode     RouteMode
	activeTeardown func()
	activeUsers    []snowflake.ID
	synthIDs       map[snowflake.ID]snowflake.ID
}

// DestSlot is the per-destination topology entry. Pipeline-writable fields:
// ChannelID, Mixer, Sources, ChOuts.
type DestSlot struct {
	ChannelID snowflake.ID
	Mixer     *opus.Mixer
	Sources   []*SourceSlot
	// ChOuts is the destination's downstream targets (speaker chOuts). When
	// the dest is in copy mode these become the active source's OpusTargets;
	// when in mix mode the mixer's SetSink writes to them.
	//
	// A nil ChOuts marks the destination as "synthetic" (relay mixer feeding
	// ally guests rather than a local voice channel); the router then skips
	// the listener check for it.
	ChOuts []chan<- []byte
}

// routeSource is the slice-shaped input to computeRoutes — one entry per
// captured source. Decoupled from SourceSlot so the pure cascade function
// can be tested without the rest of the router struct.
type routeSource struct {
	id        snowflake.ID
	channelID snowflake.ID
}

// routeDest is the slice-shaped input to computeRoutes — one entry per
// destination mixer, with the IDs of the sources that feed it.
type routeDest struct {
	channelID snowflake.ID
	sourceIDs []snowflake.ID
}

// computeRoutes is the pure cascade rule (plan §1 + §1.1).
//
// Pass 1 derives a per-source baseline mode from C(channel):
//
//	C == 0 → RouteOff, C == 1 → RouteCopy, C ≥ 2 → RouteMix.
//
// Pass 2 promotes destinations to mix when any of:
//   - At least one feeding source is already RouteMix (the C≥2 cascade), OR
//   - Two or more live (non-off) sources feed the destination. With multiple
//     copy-mode writers their OpusTargets would race on the same chOuts, so
//     the mixer is mandatory regardless of per-channel C.
//
// Every RouteCopy source feeding a promoted destination is then forced into
// RouteMix. RouteOff sources stay off — they contribute no audio so they
// cannot race with the mixer regardless of D's mode.
//
// The cascade is iterated to fixpoint. For any sane topology it converges in
// 1–2 passes; the loop guard tolerates pathological inputs without panic.
func computeRoutes(sources []routeSource, destinations []routeDest, callerCounts map[snowflake.ID]int) (sourceModes map[snowflake.ID]RouteMode, destMix map[snowflake.ID]bool) {
	sourceModes = make(map[snowflake.ID]RouteMode, len(sources))
	destMix = make(map[snowflake.ID]bool, len(destinations))

	for _, s := range sources {
		switch c := callerCounts[s.channelID]; {
		case c <= 0:
			sourceModes[s.id] = RouteOff
		case c == 1:
			sourceModes[s.id] = RouteCopy
		default:
			sourceModes[s.id] = RouteMix
		}
	}

	for changed := true; changed; {
		changed = false
		for _, d := range destinations {
			var live, mix int
			for _, sID := range d.sourceIDs {
				if sourceModes[sID] == RouteOff {
					continue
				}
				live++
				if sourceModes[sID] == RouteMix {
					mix++
				}
			}
			needsMix := mix > 0 || live >= 2
			if !needsMix {
				continue
			}
			if !destMix[d.channelID] {
				destMix[d.channelID] = true
				changed = true
			}
			for _, sID := range d.sourceIDs {
				if sourceModes[sID] == RouteCopy {
					sourceModes[sID] = RouteMix
					changed = true
				}
			}
		}
	}
	return sourceModes, destMix
}

// Router is the per-session decision engine. One per active raid; stored on
// guild.Session.AutoRouter.
//
// Thread model: every public method (Debounce, Recompute, Close) acquires mu.
// The time.Timer in debounceTimers fires its callback on its own goroutine;
// that callback also takes mu. mu IS held across handle.Install, mixer
// SetPaused, and the pipeline-supplied teardown closures inside applyModes
// — those calls are atomic stores or pure data-structure mutations and
// cannot re-enter the router. mu is released around VoiceProbe calls
// (EnumerateCallers, HasListeners) because production implementations read
// from the disgo cache, which acquires its own locks in a different order.
type Router struct {
	mu             sync.Mutex
	guildID        snowflake.ID
	roleID         snowflake.ID
	enumerator     VoiceProbe
	debounceWindow time.Duration
	sources        map[snowflake.ID]*SourceSlot // keyed by SourceSlot.ID
	destinations   map[snowflake.ID]*DestSlot   // keyed by DestSlot.ChannelID
	debounceTimers map[snowflake.ID]*time.Timer // keyed by triggering channelID
	// closed is set by Close. Recompute checks it first thing so an in-flight
	// AfterFunc that started before Close but was waiting on mu does not
	// re-install on torn-down handles. Reads/writes guarded by mu.
	closed bool
	// synthSeed is the counter used to allocate synth IDs for (source, user)
	// pairs in synthIDForLocked. Bit 63 is set on every emitted value so
	// synth IDs can never collide with real Discord snowflakes (which are
	// < 2^63).
	synthSeed uint64
	// recordTransition is invoked from applyModes on every source mode change
	// so external observability (OTel counter) can track transition rates.
	// Optional — nil is treated as a no-op so unit tests can omit it.
	recordTransition func(from, to RouteMode)
}

// New constructs a router from the topology graph. Takes ownership of the
// slices (does not copy). roleID is captured at session start; if the
// /bind-role guard is bypassed the value stays stable for the session lifetime.
func New(guildID, roleID snowflake.ID, enumerator VoiceProbe, sources []*SourceSlot, destinations []*DestSlot) *Router {
	r := &Router{
		guildID:        guildID,
		roleID:         roleID,
		enumerator:     enumerator,
		debounceWindow: defaultRouteDebounce,
		sources:        make(map[snowflake.ID]*SourceSlot, len(sources)),
		destinations:   make(map[snowflake.ID]*DestSlot, len(destinations)),
		debounceTimers: make(map[snowflake.ID]*time.Timer),
	}
	for _, s := range sources {
		r.sources[s.ID] = s
	}
	for _, d := range destinations {
		r.destinations[d.ChannelID] = d
	}
	return r
}

// WithTransitionRecorder wires an external observer (typically a telemetry
// counter) into the router. Called after New; nil-safe.
func (r *Router) WithTransitionRecorder(fn func(from, to RouteMode)) *Router {
	r.recordTransition = fn
	return r
}

// synthIDForLocked returns (and lazily allocates) the synthetic mixer-input ID
// for (s, userID). Stable for the session — once a user is given an ID, the
// same ID is reused on every subsequent install for that source so mixer
// inputs remain referenceable across transitions. Caller must hold r.mu.
func (r *Router) synthIDForLocked(s *SourceSlot, userID snowflake.ID) snowflake.ID {
	if s.synthIDs == nil {
		s.synthIDs = make(map[snowflake.ID]snowflake.ID)
	}
	if id, ok := s.synthIDs[userID]; ok {
		return id
	}
	r.synthSeed++
	// Bit 63 set ⇒ above the Discord snowflake range, no collision risk with
	// real user/bot IDs or with relayInputID / relayDestID.
	id := snowflake.ID((uint64(1) << 63) | r.synthSeed)
	s.synthIDs[userID] = id
	return id
}

// sameUserSet reports whether a and b contain the same userIDs regardless of
// order. Used to skip no-op mix-mode reinstalls when the user set is stable.
func sameUserSet(a, b []snowflake.ID) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[snowflake.ID]struct{}, len(a))
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, x := range b {
		if _, ok := seen[x]; !ok {
			return false
		}
	}
	return true
}

// Debounce schedules a Recompute triggered by an event affecting channelID.
// The timer is per affected source channel so unrelated channels don't block
// each other. Called from voice and member event handlers — must not block.
func (r *Router) Debounce(channelID snowflake.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.debounceTimers[channelID]; ok {
		t.Reset(r.debounceWindow)
		return
	}
	r.debounceTimers[channelID] = time.AfterFunc(r.debounceWindow, func() {
		r.mu.Lock()
		delete(r.debounceTimers, channelID)
		r.mu.Unlock()
		r.Recompute()
	})
}

// Recompute snapshots per-channel caller IDs from the VoiceProbe, runs the
// cascade rule, and applies the resulting mode changes. Idempotent — safe to
// call without a pending debounce (the initial route after commitSession
// calls it directly).
//
// Lock discipline: r.mu is released around the enumerator calls so that
// implementations backed by the disgo cache cannot create a lock-order edge
// against guild events. r.mu is reacquired inside applyModes.
func (r *Router) Recompute() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	// Single locked snapshot: sources, destinations, and which dests need a
	// listener probe (real dests with non-nil ChOuts; synthetic relay dests
	// skip the listener check because their consumers are ally guests).
	routeSources := make([]routeSource, 0, len(r.sources))
	uniqueChannels := make([]snowflake.ID, 0, len(r.sources))
	seenChannels := make(map[snowflake.ID]struct{}, len(r.sources))
	for _, s := range r.sources {
		routeSources = append(routeSources, routeSource{id: s.ID, channelID: s.ChannelID})
		if _, ok := seenChannels[s.ChannelID]; !ok {
			seenChannels[s.ChannelID] = struct{}{}
			uniqueChannels = append(uniqueChannels, s.ChannelID)
		}
	}
	routeDests := make([]routeDest, 0, len(r.destinations))
	listenerChecks := make([]snowflake.ID, 0, len(r.destinations))
	for chID, d := range r.destinations {
		ids := make([]snowflake.ID, 0, len(d.Sources))
		for _, s := range d.Sources {
			ids = append(ids, s.ID)
		}
		routeDests = append(routeDests, routeDest{channelID: chID, sourceIDs: ids})
		if len(d.ChOuts) > 0 {
			listenerChecks = append(listenerChecks, chID)
		}
	}
	roleID := r.roleID
	r.mu.Unlock()

	usersPerChannel := make(map[snowflake.ID][]snowflake.ID, len(uniqueChannels))
	callerCounts := make(map[snowflake.ID]int, len(uniqueChannels))
	for _, chID := range uniqueChannels {
		users := r.enumerator.EnumerateCallers(chID, roleID)
		usersPerChannel[chID] = users
		callerCounts[chID] = len(users)
	}
	listenersPerChannel := make(map[snowflake.ID]bool, len(listenerChecks))
	for _, chID := range listenerChecks {
		listenersPerChannel[chID] = r.enumerator.HasListeners(chID)
	}

	sourceModes, destMix := computeRoutes(routeSources, routeDests, callerCounts)
	r.applyModes(sourceModes, destMix, usersPerChannel, listenersPerChannel)
}

// applyModes is the second half of Recompute. For each source it decides
// whether a reinstall is needed — mode change OR (mix mode AND user set
// changed) — runs the previous install's teardown, calls the pipeline-
// supplied BuildInstall closure, and atomically swaps the new spec in via
// handle.Install. Destination mixer pause state is updated to match destMix.
//
// Lock discipline: r.mu is held throughout. None of the calls performed under
// the lock (Install, SetPaused, the pipeline's teardown closures) acquire
// external locks in the same order as r.mu, so no inversion is possible.
func (r *Router) applyModes(sourceModes map[snowflake.ID]RouteMode, destMix map[snowflake.ID]bool, usersPerChannel map[snowflake.ID][]snowflake.ID, listenersPerChannel map[snowflake.ID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, newMode := range sourceModes {
		s, ok := r.sources[id]
		if !ok {
			continue
		}
		users := usersPerChannel[s.ChannelID]
		needInstall := s.activeMode != newMode
		if !needInstall && newMode == RouteMix && !sameUserSet(s.activeUsers, users) {
			needInstall = true
		}
		if !needInstall {
			continue
		}
		slog.Debug("auto-route: source reinstall",
			slog.String("guildID", r.guildID.String()),
			slog.String("sourceID", id.String()),
			slog.String("channelID", s.ChannelID.String()),
			slog.Int("feeds", len(s.Feeds)),
			slog.Int("users", len(users)),
			slog.String("from", s.activeMode.String()),
			slog.Int("from_raw", int(s.activeMode)),
			slog.String("to", newMode.String()),
			slog.Int("to_raw", int(newMode)),
		)
		if s.activeTeardown != nil {
			s.activeTeardown()
			s.activeTeardown = nil
		}
		s.activeUsers = nil
		if s.Handle != nil && s.BuildInstall != nil {
			bindings := r.bindUsersLocked(s, users, newMode)
			spec, teardown := s.BuildInstall(newMode, bindings)
			s.Handle.Install(spec)
			s.activeTeardown = teardown
			if newMode == RouteMix {
				snap := make([]snowflake.ID, len(users))
				copy(snap, users)
				s.activeUsers = snap
			}
		}
		// Only record a transition when the mode actually changed; mix-mode
		// user-set churn (which also triggers a reinstall) is not a routing
		// transition and would otherwise drown out the signal.
		if r.recordTransition != nil && s.activeMode != newMode {
			r.recordTransition(s.activeMode, newMode)
		}
		s.activeMode = newMode
		// Prune synth IDs for users no longer present in the source channel.
		// They will be reallocated if/when the user rejoins. Pruning here (in
		// the reinstall path only) is safe because user-set changes force a
		// reinstall via the sameUserSet check above — so any voice-leave
		// reaches this branch within one debounce window.
		pruneSynthIDsLocked(s, users)
	}
	for chID, d := range r.destinations {
		if d.Mixer == nil {
			continue
		}
		// Pause when the cascade has nothing to mix OR when the destination
		// has no human listener. Synthetic destinations (relay; ChOuts nil)
		// are absent from listenersPerChannel and default to "has listeners"
		// since their consumers are ally guests, not local users.
		shouldRun := destMix[chID]
		if shouldRun && len(d.ChOuts) > 0 {
			if !listenersPerChannel[chID] {
				shouldRun = false
			}
		}
		d.Mixer.SetPaused(!shouldRun)
	}
}

// bindUsersLocked allocates a UserBinding for each user in mix mode. Returns
// nil for non-mix modes — BuildInstall ignores the slice for off/copy.
func (r *Router) bindUsersLocked(s *SourceSlot, users []snowflake.ID, mode RouteMode) []UserBinding {
	if mode != RouteMix || len(users) == 0 {
		return nil
	}
	out := make([]UserBinding, 0, len(users))
	for _, u := range users {
		out = append(out, UserBinding{UserID: u, SynthID: r.synthIDForLocked(s, u)})
	}
	return out
}

// pruneSynthIDsLocked removes synth IDs for users no longer present in the
// source channel. Bounds the per-source synthIDs map at "users currently in
// the channel" instead of "every user who ever joined". Caller must hold
// r.mu (it's only called from applyModes, which holds the lock throughout).
func pruneSynthIDsLocked(s *SourceSlot, presentUsers []snowflake.ID) {
	if len(s.synthIDs) == 0 {
		return
	}
	present := make(map[snowflake.ID]struct{}, len(presentUsers))
	for _, u := range presentUsers {
		present[u] = struct{}{}
	}
	for u := range s.synthIDs {
		if _, ok := present[u]; !ok {
			delete(s.synthIDs, u)
		}
	}
}

// ScheduleRecompute fires a one-shot Recompute after delay. Used by
// pipelines at session start to catch voice-state updates that arrive after
// the initial synchronous Recompute — e.g. a user already in the owner
// channel whose VOICE_STATE_UPDATE was still in flight through the gateway
// when the pipeline committed.
//
// The timer is registered the same way as debounce timers, so Close() will
// stop it cleanly if the session tears down before the followup fires.
// channelID is a synthetic key (delay nanoseconds) so it never collides with
// real voice-event debounce timers.
func (r *Router) ScheduleRecompute(delay time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	key := snowflake.ID(delay.Nanoseconds()) | (snowflake.ID(1) << 62)
	if t, ok := r.debounceTimers[key]; ok {
		t.Reset(delay)
		return
	}
	r.debounceTimers[key] = time.AfterFunc(delay, func() {
		r.mu.Lock()
		delete(r.debounceTimers, key)
		r.mu.Unlock()
		r.Recompute()
	})
}

// Close stops every pending debounce timer, marks the router closed so any
// AfterFunc that fires after this point bails out of Recompute, and runs
// every active source's teardown closure to release mixer inputs allocated
// during the last install. Call from session-end teardown before closing
// FanoutHandles.
func (r *Router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for chID, t := range r.debounceTimers {
		t.Stop()
		delete(r.debounceTimers, chID)
	}
	for _, s := range r.sources {
		if s.activeTeardown != nil {
			s.activeTeardown()
			s.activeTeardown = nil
		}
	}
}
