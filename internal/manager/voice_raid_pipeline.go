package manager

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// pipelineParams holds all inputs that the three host pipeline topologies need.
// Built once by StartVoiceRaid after the common owner/speaker setup; passed to hostPipeline.build.
type pipelineParams struct {
	guildID      snowflake.ID
	ownerBotID   snowflake.ID
	cancelFunc   context.CancelFunc
	mode         guild.RaidMode
	allyCode     ally.Code
	allySession  *ally.Session
	setup        *raidSetup
	ownerHandle  *opus.FanoutHandle // owner FanoutHandle; non-nil when owner capture is enabled
	chOwnerOut   chan []byte        // owner playback channel; nil for direct passthrough (RaidModeOneCaller)
	ownerCleanup func()             // closes owner provider/receiver; called on teardown or build error
	ov           pool.GuildVoice
	gm           telemetry.GuildMetrics
	allowFilter  *AllowFilter
	voiceProbe   VoiceProbe // production: *cacheVoiceProbe; consumed by the auto-router
}

// hostPipeline builds the audio wiring for one topology and returns the
// session to commit plus a start func to call after commitSession. start()
// is responsible for spawning mixer goroutines and seeding initial pause
// state via the router's Recompute.
// On failure it returns a non-nil error; the caller is responsible for cleaning up
// speakers, the owner voice connection, and the ally session.
type hostPipeline interface {
	build(ctx context.Context, p pipelineParams) (*guild.Session, func(), error)
}

// pipelineFor returns the correct pipeline implementation for mode.
//
// All three host modes now run through the always-on mixer graph + auto router
// (oneCallerPipeline / guildCallerPipeline / starCallerPipeline).
func pipelineFor(mode guild.RaidMode) hostPipeline {
	switch {
	case mode.IsDirectPassthrough():
		return oneCallerPipeline{}
	case mode.IsStarTopology():
		return starCallerPipeline{}
	default:
		return guildCallerPipeline{}
	}
}
