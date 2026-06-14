package manager

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

// routeMode is the per-source dispatch decision made by the router.
type routeMode uint8

const (
	// routeOff: no role-bearing callers are present in the source channel.
	// FanoutHandle installed with empty targets; received frames are dropped.
	routeOff routeMode = iota
	// routeCopy: exactly one caller; frames flow through OpusTargets without
	// decode. Mixer is paused; one source writes directly to chOuts.
	routeCopy
	// routeMix: two or more callers, OR a per-destination cascade has forced
	// the source into mix mode because a peer destination requires it.
	// Frames are decoded and fed into the destination mixers' SourceBuffers.
	routeMix
)

// VoiceProbe is the read-only window into per-channel voice state that the
// auto-router needs. Implementations must be safe to call concurrently and
// must not retain the returned slice. The router never holds its own mu
// across a probe call.
//
// EnumerateCallers feeds the cascade rule (via len()) and the buildInstall
// closures (so each role-bearing user gets their own SourceBuffer per
// destination mixer — §4.3 fix).
//
// HasListeners answers "should this destination's mixer actually run?". The
// router pauses a destination's mixer when either the cascade says so OR no
// human listener is in the destination channel. Synthetic destinations
// (relay mixer; chOuts == nil) skip this check — their consumers are ally
// guests, not local voice-channel humans.
//
// The production implementation reads from disgo's voice-state and member
// caches; tests provide an in-memory fake.
type VoiceProbe interface {
	EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID
	HasListeners(channelID snowflake.ID) bool
}

// routeSource is the slice-shaped input to computeRoutes — one entry per
// captured source (a VoiceReceiver feeding the router). Decoupled from
// sourceSlot so the pure cascade function can be tested without the rest of
// the router struct.
type routeSource struct {
	id        snowflake.ID // bot user ID of the capturing receiver (owner bot or speaker)
	channelID snowflake.ID // voice channel that this source captures from
}

// routeDest is the slice-shaped input to computeRoutes — one entry per
// destination mixer in the topology, with the IDs of the sources that feed it.
type routeDest struct {
	channelID snowflake.ID
	sourceIDs []snowflake.ID
}

// computeRoutes is the pure cascade rule (plan §1 + §1.1).
//
// Pass 1 derives a per-source baseline mode from C(channel):
//
//	C == 0 → routeOff, C == 1 → routeCopy, C ≥ 2 → routeMix.
//
// Pass 2 promotes destinations to mix when any of:
//   - At least one feeding source is already routeMix (the C≥2 cascade), OR
//   - Two or more live (non-off) sources feed the destination. With multiple
//     copy-mode writers their OpusTargets would race on the same chOuts, so
//     the mixer is mandatory regardless of per-channel C.
//
// Every routeCopy source feeding a promoted destination is then forced into
// routeMix. routeOff sources stay off — they contribute no audio so they
// cannot race with the mixer regardless of D's mode.
//
// The cascade is iterated to fixpoint. For any sane topology it converges in
// 1–2 passes; the loop guard tolerates pathological inputs without panic.
func computeRoutes(sources []routeSource, destinations []routeDest, callerCounts map[snowflake.ID]int) (sourceModes map[snowflake.ID]routeMode, destMix map[snowflake.ID]bool) {
	sourceModes = make(map[snowflake.ID]routeMode, len(sources))
	destMix = make(map[snowflake.ID]bool, len(destinations))

	for _, s := range sources {
		switch c := callerCounts[s.channelID]; {
		case c <= 0:
			sourceModes[s.id] = routeOff
		case c == 1:
			sourceModes[s.id] = routeCopy
		default:
			sourceModes[s.id] = routeMix
		}
	}

	for changed := true; changed; {
		changed = false
		for _, d := range destinations {
			var live, mix int
			for _, sID := range d.sourceIDs {
				if sourceModes[sID] == routeOff {
					continue
				}
				live++
				if sourceModes[sID] == routeMix {
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
				if sourceModes[sID] == routeCopy {
					sourceModes[sID] = routeMix
					changed = true
				}
			}
		}
	}
	return sourceModes, destMix
}

// userBinding pairs a userID with the synthetic mixer-input ID the router
// allocates for that user against a given source. Synth IDs let two different
// users in the same source channel each have their own SourceBuffer in every
// destination mixer (the §4.3 fix), without colliding with real Discord
// snowflakes (synth IDs have bit 63 set; Discord snowflakes don't).
type userBinding struct {
	userID  snowflake.ID
	synthID snowflake.ID
}

// sourceSlot is the router-owned per-source state. Holds the FanoutHandle the
// router will re-Install on transitions plus a pipeline-supplied closure that
// constructs the per-mode FanoutInstall spec — keeping the router free of
// topology-specific knowledge (e.g. relay broadcast in OneCaller copy mode).
type sourceSlot struct {
	id        snowflake.ID
	channelID snowflake.ID
	handle    *opus.FanoutHandle
	feeds     []*destSlot
	// mode is the most recently applied routing decision for this source.
	mode routeMode
	// buildInstall is supplied by the pipeline. It returns the FanoutInstall
	// for the requested mode plus a teardown closure that releases any
	// mixer inputs or SourceBuffer allocations made by this install. The
	// users slice is only populated for routeMix; copy/off modes ignore it.
	buildInstall func(mode routeMode, users []userBinding) (opus.FanoutInstall, func())
	// activeTeardown holds the teardown closure returned by the last
	// buildInstall call. Invoked before the next install and once at
	// session-end teardown.
	activeTeardown func()
	// activeUsers is the user set the last install was built for. Mix-mode
	// reinstalls are triggered when this differs from the freshly enumerated
	// list even if the cascade mode stays the same.
	activeUsers []snowflake.ID
	// synthIDs caches the synth ID allocated for each user against this
	// source. Stable across re-installs so a user's mixer inputs keep the
	// same key throughout the session.
	synthIDs map[snowflake.ID]snowflake.ID
}

// destSlot is the router-owned per-destination state.
type destSlot struct {
	channelID snowflake.ID
	mixer     *opus.Mixer
	sources   []*sourceSlot
	// chOuts is the destination's downstream targets (speaker chOuts). When
	// the dest is in copy mode these become the active source's OpusTargets;
	// when in mix mode the mixer's SetSink writes to them.
	chOuts []chan<- []byte
}

// sourceRouter is the per-session decision engine. One per active raid; lives
// on guild.Session.AutoRouter.
//
// Thread model: every public method (Debounce, Recompute, Close) acquires mu.
// The time.Timer in debounceTimers fires its callback on its own goroutine;
// that callback also takes mu. mu IS held across handle.Install, mixer
// SetPaused, and the pipeline-supplied teardown closures inside applyModes
// — those calls are atomic stores or pure data-structure mutations and
// cannot re-enter the router. mu is released around VoiceProbe calls
// (EnumerateCallers, HasListeners) because production implementations read
// from the disgo cache, which acquires its own locks in a different order.
type sourceRouter struct {
	mu             sync.Mutex
	guildID        snowflake.ID
	roleID         snowflake.ID
	enumerator     VoiceProbe
	debounceWindow time.Duration
	sources        map[snowflake.ID]*sourceSlot // keyed by source.id
	destinations   map[snowflake.ID]*destSlot   // keyed by destination channelID
	debounceTimers map[snowflake.ID]*time.Timer // keyed by triggering channelID
	// closed is set by Close. Recompute checks it first thing so an in-flight
	// AfterFunc that started before Close but was waiting on mu does not
	// re-install on torn-down handles. Reads/writes guarded by mu.
	closed bool
	// synthSeed is the counter used to allocate synth IDs for (source, user)
	// pairs in synthIDFor. Bit 63 is set on every emitted value so synth IDs
	// can never collide with real Discord snowflakes (which are < 2^63).
	synthSeed uint64
	// recordTransition is invoked from applyModes on every source mode change
	// so external observability (OTel counter) can track transition rates.
	// Optional — nil is treated as a no-op so unit tests can omit it.
	recordTransition func(from, to routeMode)
}

// newSourceRouter constructs a router from the topology graph. Takes ownership
// of the slices (does not copy). roleID is captured at session start; if the
// /bind-role guard is bypassed the value stays stable for the session lifetime.
func newSourceRouter(guildID, roleID snowflake.ID, enumerator VoiceProbe, sources []*sourceSlot, destinations []*destSlot) *sourceRouter {
	r := &sourceRouter{
		guildID:        guildID,
		roleID:         roleID,
		enumerator:     enumerator,
		debounceWindow: defaultRouteDebounce,
		sources:        make(map[snowflake.ID]*sourceSlot, len(sources)),
		destinations:   make(map[snowflake.ID]*destSlot, len(destinations)),
		debounceTimers: make(map[snowflake.ID]*time.Timer),
	}
	for _, s := range sources {
		r.sources[s.id] = s
	}
	for _, d := range destinations {
		r.destinations[d.channelID] = d
	}
	return r
}

// withTransitionRecorder wires an external observer (typically a telemetry
// counter) into the router. Called after newSourceRouter; nil-safe.
func (r *sourceRouter) withTransitionRecorder(fn func(from, to routeMode)) *sourceRouter {
	r.recordTransition = fn
	return r
}

// synthIDForLocked returns (and lazily allocates) the synthetic mixer-input ID
// for (s, userID). Stable for the session — once a user is given an ID, the
// same ID is reused on every subsequent install for that source so mixer
// inputs remain referenceable across transitions. Caller must hold r.mu.
func (r *sourceRouter) synthIDForLocked(s *sourceSlot, userID snowflake.ID) snowflake.ID {
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
func (r *sourceRouter) Debounce(channelID snowflake.ID) {
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

// Recompute snapshots per-channel caller IDs from the VoiceProbe, runs
// the cascade rule, and applies the resulting mode changes. Idempotent — safe
// to call without a pending debounce (the initial route after commitSession
// calls it directly).
//
// Lock discipline: r.mu is released around the enumerator calls so that
// implementations backed by the disgo cache cannot create a lock-order edge
// against guild events. r.mu is reacquired inside applyModes.
func (r *sourceRouter) Recompute() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	routeSources := make([]routeSource, 0, len(r.sources))
	uniqueChannels := make([]snowflake.ID, 0, len(r.sources))
	seenChannels := make(map[snowflake.ID]struct{}, len(r.sources))
	for _, s := range r.sources {
		routeSources = append(routeSources, routeSource{id: s.id, channelID: s.channelID})
		if _, ok := seenChannels[s.channelID]; !ok {
			seenChannels[s.channelID] = struct{}{}
			uniqueChannels = append(uniqueChannels, s.channelID)
		}
	}
	routeDests := make([]routeDest, 0, len(r.destinations))
	for _, d := range r.destinations {
		ids := make([]snowflake.ID, 0, len(d.sources))
		for _, s := range d.sources {
			ids = append(ids, s.id)
		}
		routeDests = append(routeDests, routeDest{channelID: d.channelID, sourceIDs: ids})
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

	// Probe listeners on every real destination (chOuts non-empty). Synthetic
	// destinations (relay; chOuts == nil) skip the check — their consumers
	// are ally guests, tracked elsewhere.
	r.mu.Lock()
	listenerChecks := make([]snowflake.ID, 0, len(r.destinations))
	for chID, d := range r.destinations {
		if len(d.chOuts) > 0 {
			listenerChecks = append(listenerChecks, chID)
		}
	}
	r.mu.Unlock()
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
// supplied buildInstall closure, and atomically swaps the new spec in via
// handle.Install. Destination mixer pause state is updated to match destMix.
//
// Lock discipline: r.mu is held throughout. None of the calls performed under
// the lock (Install, SetPaused, the pipeline's teardown closures) acquire
// external locks in the same order as r.mu, so no inversion is possible.
func (r *sourceRouter) applyModes(sourceModes map[snowflake.ID]routeMode, destMix map[snowflake.ID]bool, usersPerChannel map[snowflake.ID][]snowflake.ID, listenersPerChannel map[snowflake.ID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, newMode := range sourceModes {
		s, ok := r.sources[id]
		if !ok {
			continue
		}
		users := usersPerChannel[s.channelID]
		needInstall := s.mode != newMode
		if !needInstall && newMode == routeMix && !sameUserSet(s.activeUsers, users) {
			needInstall = true
		}
		if !needInstall {
			continue
		}
		slog.Debug("auto-route: source reinstall",
			slog.String("guildID", r.guildID.String()),
			slog.String("sourceID", id.String()),
			slog.String("channelID", s.channelID.String()),
			slog.Int("feeds", len(s.feeds)),
			slog.Int("users", len(users)),
			slog.String("from", s.mode.String()),
			slog.Int("from_raw", int(s.mode)),
			slog.String("to", newMode.String()),
			slog.Int("to_raw", int(newMode)),
		)
		if s.activeTeardown != nil {
			s.activeTeardown()
			s.activeTeardown = nil
		}
		s.activeUsers = nil
		if s.handle != nil && s.buildInstall != nil {
			bindings := r.bindUsersLocked(s, users, newMode)
			spec, teardown := s.buildInstall(newMode, bindings)
			s.handle.Install(spec)
			s.activeTeardown = teardown
			if newMode == routeMix {
				snap := make([]snowflake.ID, len(users))
				copy(snap, users)
				s.activeUsers = snap
			}
		}
		// Only record a transition when the mode actually changed; mix-mode
		// user-set churn (which also triggers a reinstall) is not a routing
		// transition and would otherwise drown out the signal.
		if r.recordTransition != nil && s.mode != newMode {
			r.recordTransition(s.mode, newMode)
		}
		s.mode = newMode
	}
	for chID, d := range r.destinations {
		if d.mixer == nil {
			continue
		}
		// Pause when the cascade has nothing to mix OR when the destination
		// has no human listener. Synthetic destinations (relay; chOuts nil)
		// are absent from listenersPerChannel and default to "has listeners"
		// since their consumers are ally guests, not local users.
		shouldRun := destMix[chID]
		if shouldRun && len(d.chOuts) > 0 {
			if !listenersPerChannel[chID] {
				shouldRun = false
			}
		}
		d.mixer.SetPaused(!shouldRun)
	}
}

// bindUsersLocked allocates a userBinding for each user in mix mode. Returns
// nil for non-mix modes — buildInstall ignores the slice for off/copy.
func (r *sourceRouter) bindUsersLocked(s *sourceSlot, users []snowflake.ID, mode routeMode) []userBinding {
	if mode != routeMix || len(users) == 0 {
		return nil
	}
	out := make([]userBinding, 0, len(users))
	for _, u := range users {
		out = append(out, userBinding{userID: u, synthID: r.synthIDForLocked(s, u)})
	}
	return out
}

// String renders routeMode for slog and test failures.
func (m routeMode) String() string {
	switch m {
	case routeOff:
		return "off"
	case routeCopy:
		return "copy"
	case routeMix:
		return "mix"
	default:
		return "unknown"
	}
}

// Close stops every pending debounce timer, marks the router closed so any
// AfterFunc that fires after this point bails out of Recompute, and runs
// every active source's teardown closure to release mixer inputs allocated
// during the last install. Call from session-end teardown before closing
// FanoutHandles.
func (r *sourceRouter) Close() {
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
