// Package pipeline builds the audio topology for each raid mode and wires it
// to the auto-router. Host and guest pipelines share a common shape: each
// constructs router.SourceSlot / router.DestSlot graphs, supplies a
// BuildInstall closure per source, and hands the constructed Session back to
// the manager-package Service for lifecycle ownership.
//
// All five router-driven pipelines (oneCaller, guildCaller, starCaller,
// guestCaller, guestStarCaller) live here. The listener guest pipeline does
// not need the router (no capture) and is a degenerate case.
package pipeline

import (
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/pool"
)

// RelayInputID is the synthetic source ID used when adding a guest relay feed
// as an input to a host-side ChannelMixer. Discord snowflakes are epoch-based
// (minimum value ~4 billion) so 1 never collides with a real user/bot ID.
const RelayInputID snowflake.ID = 1

// RelayDestID is the synthetic destination channelID used to model the ally
// relay mixer as a router.DestSlot. Discord snowflakes are epoch-based
// (minimum ~4×10¹⁰); any small integer sits safely below that range.
// RelayInputID uses 1, so 2 is the next safe choice.
const RelayDestID snowflake.ID = 2

// VoiceLeaveTimeout caps how long a voice.Conn.Leave call may block. Used by
// the speaker-cleanup goroutine spawned in BuildSpeakerCleanup.
const VoiceLeaveTimeout = 5 * time.Second

// SpeakerResult holds the outcome of a single successfully joined speaker.
// Constructed by manager.Service.setupSpeakers and consumed by pipelines via
// Setup.
type SpeakerResult struct {
	Speaker guild.Speaker
	ChOut   chan<- []byte
	Handle  *opus.FanoutHandle // nil when withCapture is false
	GV      pool.GuildVoice
	Cleanup func() // closes provider/receiver; caller must invoke on teardown
}

// Setup captures the common setup result for both host and guest flows.
// Returned by manager.Service.setupSpeakers; passed verbatim into HostFor /
// GuestFor pipeline builders via Params/GuestParams.
type Setup struct {
	Joined         []SpeakerResult
	Speakers       []guild.Speaker
	SpeakerCleanup func()
	Outs           []chan<- []byte
}

// SourceEntry is one audio capture source feeding the router graph. Handle
// is non-nil when the source's VoiceReceiver dispatches via FanoutHandle
// (inline fanout mode).
type SourceEntry struct {
	ID        snowflake.ID
	ChannelID snowflake.ID
	Handle    *opus.FanoutHandle
}

// DestChannel groups all speaker output channels that share the same voice
// channel. Built by BuildDestinations from the SpeakerResult list.
type DestChannel struct {
	ChannelID snowflake.ID
	Outs      []chan<- []byte
}

// mixerRef pairs a mixer with the input ID registered in it. Used by the
// router-driven install closures to detach the mixer inputs they allocated
// when transitioning out of mix mode or closing the session.
type mixerRef struct {
	mx *opus.Mixer
	id snowflake.ID
}
