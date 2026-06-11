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

// CallerCounter reports the number of role-bearing callers currently in
// channelID. The router calls this on every Recompute to drive routing
// decisions. The production implementation reads from disgo's voice-state and
// member caches; tests provide an in-memory fake.
type CallerCounter interface {
	CountCallers(channelID, roleID snowflake.ID) int
}

// routeSource is the slice-shaped input to computeRoutes — one entry per
// captured source (a VoiceReceiver feeding the router). Decoupled from
// sourceSlot so the pure cascade function can be tested without the rest of
// the router struct.
type routeSource struct {
	id        snowflake.ID // stable source identifier (current owner uses channelID)
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
	// mixer inputs or SourceBuffer allocations made by this install.
	// applyModes calls the prior teardown immediately before invoking
	// buildInstall for the next mode.
	buildInstall func(mode routeMode) (opus.FanoutInstall, func())
	// activeTeardown holds the teardown closure returned by the last
	// buildInstall call. Invoked before the next install and once at
	// session-end teardown.
	activeTeardown func()
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
// Thread model: every public method (Debounce, Recompute) acquires mu. The
// time.Timer in debounceTimers fires its callback on its own goroutine; that
// callback also takes mu. mu is never held across a handle.Install or
// mixer.SetPaused call to avoid unbounded blocking.
type sourceRouter struct {
	mu             sync.Mutex
	guildID        snowflake.ID
	roleID         snowflake.ID
	counter        CallerCounter
	debounceWindow time.Duration
	sources        map[snowflake.ID]*sourceSlot // keyed by source.id
	destinations   map[snowflake.ID]*destSlot   // keyed by destination channelID
	debounceTimers map[snowflake.ID]*time.Timer // keyed by triggering channelID
	// channelToSources indexes which sources observe a given channelID, so
	// Debounce can wake the right entries on a voice event.
	channelToSources map[snowflake.ID][]*sourceSlot
}

// newSourceRouter constructs a router from the topology graph. Takes ownership
// of the slices (does not copy). roleID is captured at session start; if the
// /bind-role guard is bypassed the value stays stable for the session lifetime.
func newSourceRouter(guildID, roleID snowflake.ID, counter CallerCounter, sources []*sourceSlot, destinations []*destSlot) *sourceRouter {
	r := &sourceRouter{
		guildID:          guildID,
		roleID:           roleID,
		counter:          counter,
		debounceWindow:   defaultRouteDebounce,
		sources:          make(map[snowflake.ID]*sourceSlot, len(sources)),
		destinations:     make(map[snowflake.ID]*destSlot, len(destinations)),
		debounceTimers:   make(map[snowflake.ID]*time.Timer),
		channelToSources: make(map[snowflake.ID][]*sourceSlot, len(sources)),
	}
	for _, s := range sources {
		r.sources[s.id] = s
		r.channelToSources[s.channelID] = append(r.channelToSources[s.channelID], s)
	}
	for _, d := range destinations {
		r.destinations[d.channelID] = d
	}
	return r
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

// Recompute snapshots per-channel caller counts from the CallerCounter, runs
// the cascade rule, and applies the resulting mode changes. Idempotent — safe
// to call without a pending debounce (the initial route after commitSession
// calls it directly).
//
// Lock discipline: r.mu is released around the CallerCounter call so that
// counter implementations backed by the disgo cache cannot create a lock-
// order edge against guild events. r.mu is reacquired inside applyModes.
func (r *sourceRouter) Recompute() {
	r.mu.Lock()
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

	callerCounts := make(map[snowflake.ID]int, len(uniqueChannels))
	for _, chID := range uniqueChannels {
		callerCounts[chID] = r.counter.CountCallers(chID, roleID)
	}

	sourceModes, destMix := computeRoutes(routeSources, routeDests, callerCounts)
	r.applyModes(sourceModes, destMix)
}

// applyModes is the second half of Recompute. For each source whose mode
// changed it runs the previous install's teardown, calls the pipeline-
// supplied buildInstall closure to obtain the new spec + teardown, and
// atomically swaps the spec in via handle.Install. Destination mixer pause
// state is updated to match destMix.
//
// Lock discipline: r.mu is held throughout. None of the calls performed under
// the lock (Install, SetPaused, the pipeline's teardown closures) acquire
// external locks in the same order as r.mu, so no inversion is possible.
func (r *sourceRouter) applyModes(sourceModes map[snowflake.ID]routeMode, destMix map[snowflake.ID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, newMode := range sourceModes {
		s, ok := r.sources[id]
		if !ok {
			continue
		}
		if s.mode == newMode {
			continue
		}
		slog.Debug("auto-route: source mode change",
			slog.String("guildID", r.guildID.String()),
			slog.String("sourceID", id.String()),
			slog.String("channelID", s.channelID.String()),
			slog.Int("feeds", len(s.feeds)),
			slog.String("from", s.mode.String()),
			slog.String("to", newMode.String()),
		)
		if s.activeTeardown != nil {
			s.activeTeardown()
			s.activeTeardown = nil
		}
		if s.handle != nil && s.buildInstall != nil {
			spec, teardown := s.buildInstall(newMode)
			s.handle.Install(spec)
			s.activeTeardown = teardown
		}
		s.mode = newMode
	}
	for chID, d := range r.destinations {
		if d.mixer == nil {
			continue
		}
		d.mixer.SetPaused(!destMix[chID])
	}
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

// Close stops every pending debounce timer and runs every active source's
// teardown closure to release mixer inputs allocated during the last install.
// Call from session-end teardown before closing FanoutHandles.
func (r *sourceRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
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
