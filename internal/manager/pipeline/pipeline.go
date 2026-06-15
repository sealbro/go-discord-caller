package pipeline

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/manager/router"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// Params holds all inputs the three host pipeline topologies need.
// Built once by manager.Service.StartVoiceRaid after the common owner/speaker setup;
// passed verbatim to HostPipeline.Build.
type Params struct {
	GuildID      snowflake.ID
	OwnerBotID   snowflake.ID
	CancelFunc   context.CancelFunc
	Mode         guild.RaidMode
	AllyCode     ally.Code
	AllySession  *ally.Session
	Setup        *Setup
	OwnerHandle  *opus.FanoutHandle // owner FanoutHandle; non-nil when owner capture is enabled
	ChOwnerOut   chan []byte        // owner playback channel; nil for direct passthrough (RaidModeOneCaller)
	OwnerCleanup func()             // closes owner provider/receiver; called on teardown or build error
	OV           pool.GuildVoice
	GM           telemetry.GuildMetrics
	// RoleID is the capture role used by AllowFilter; supplied separately so
	// pipeline does not need to import the manager package for the concrete
	// AllowFilter type.
	RoleID      snowflake.ID
	AllowFilter guild.AllowUpdater
	VoiceProbe  router.VoiceProbe // production: *manager.cacheVoiceProbe
}

// GuestParams holds all inputs the three guest pipeline topologies need.
// Built once by manager.Service.JoinSession after the common owner/speaker setup.
type GuestParams struct {
	GuestGuildID   snowflake.ID
	OwnerBotID     snowflake.ID
	OwnerChannelID snowflake.ID // zero when owner bot could not join
	CancelFunc     context.CancelFunc
	Code           ally.Code
	GuestMode      guild.RaidMode
	AllySession    *ally.Session
	Setup          *Setup
	OwnerChOut     chan []byte
	OwnerHandle    *opus.FanoutHandle // non-nil iff the guest owner bot has an inline-capture VoiceReceiver wired up
	GuestGM        telemetry.GuildMetrics
	RoleID         snowflake.ID
	AllowFilter    guild.AllowUpdater
	VoiceProbe     router.VoiceProbe
}

// HostPipeline builds the audio wiring for one host topology and returns the
// session to commit plus a start func to call after commitSession. start()
// is responsible for spawning mixer goroutines and seeding initial pause
// state via the router's Recompute.
// On failure it returns a non-nil error; the caller is responsible for cleaning up
// speakers, the owner voice connection, and the ally session.
type HostPipeline interface {
	Build(ctx context.Context, p Params) (*guild.Session, func(), error)
}

// GuestPipeline builds the audio wiring for one guest topology and returns
// the session, a start func to call after commitSession, a cleanup func to
// call on teardown, and an error. On failure the caller is responsible for
// running speaker/owner cleanup and removing the guest from the session
// registry.
type GuestPipeline interface {
	Build(ctx context.Context, p GuestParams) (session *guild.Session, start func(), cleanup func(), err error)
}

// HostFor returns the correct host pipeline implementation for mode.
//
// All three host modes run through the always-on mixer graph + auto router
// (OneCallerPipeline / GuildCallerPipeline / StarCallerPipeline).
func HostFor(mode guild.RaidMode) HostPipeline {
	switch {
	case mode.IsDirectPassthrough():
		return OneCallerPipeline{}
	case mode.IsStarTopology():
		return StarCallerPipeline{}
	default:
		return GuildCallerPipeline{}
	}
}

// GuestFor returns the correct guest pipeline implementation for mode.
func GuestFor(mode guild.RaidMode) GuestPipeline {
	switch {
	case !mode.WithCapture():
		return GuestListenerPipeline{}
	case mode.IsStarTopology():
		return GuestStarCallerPipeline{}
	default:
		return GuestCallerPipeline{}
	}
}
