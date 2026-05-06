package manager

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

// VoiceConnSetup is a builder that configures a voice connection's provider
// and receiver pair. Use the With* methods to select the desired mode, then
// call Apply to wire everything into a voice.Conn.
type VoiceConnSetup struct {
	userID     snowflake.ID
	providerFn func(chIn <-chan []byte) (voice.OpusFrameProvider, error)
	receiverFn func() (chan []byte, voice.OpusFrameReceiver, *opus.FanoutHandle, error)
}

// NewVoiceConnSetup creates a new voice session builder.
func NewVoiceConnSetup(userID snowflake.ID) *VoiceConnSetup {
	return &VoiceConnSetup{userID: userID}
}

// WithVoiceProvider reads opus frames from chIn and plays them.
// metrics carries both the histogram recorder and (optionally) the drop callback —
// build it via GuildMetrics.Provider() to wire both in one shot.
func (v *VoiceConnSetup) WithVoiceProvider(metrics telemetry.OpusRecorder) *VoiceConnSetup {
	v.providerFn = func(chIn <-chan []byte) (voice.OpusFrameProvider, error) {
		return opus.NewVoiceProvider(chIn, metrics), nil
	}
	return v
}

// WithVoiceReceiver captures incoming voice frames filtered by allowUser.
// metrics carries both the histogram recorder and (optionally) the drop callback —
// build it via GuildMetrics.Receiver() to wire both in one shot.
//
// A FanoutHandle is created and attached to the receiver so the wiring code
// can later call handle.Install with the topology-specific targets, switching
// the receiver from legacy chan-bytes mode to inline decode + multicast mode.
// Topologies that do NOT need decode (RaidModeOneCaller direct passthrough)
// simply never call Install; the receiver falls back to the chan path.
func (v *VoiceConnSetup) WithVoiceReceiver(allowUser func(snowflake.ID) bool, metrics telemetry.OpusRecorder) *VoiceConnSetup {
	v.receiverFn = func() (chan []byte, voice.OpusFrameReceiver, *opus.FanoutHandle, error) {
		ch := make(chan []byte, audioChanBuf)
		handle := opus.NewFanoutHandle()
		return ch, opus.NewVoiceReceiver(ch, v.userID, allowUser, metrics, handle), handle, nil
	}
	return v
}

// Apply configures the voice connection with the session's provider and receiver,
// sets the speaking flag, and returns the capture output channel (nil when no
// capture is configured), the FanoutHandle (nil when no receiver is configured),
// and a cleanup function.
//
// The handle must be passed to the wiring code so it can call handle.Install
// once the topology is built. The cleanup function calls handle.Close() so
// session-end teardown fires the install-time OnClose hook (e.g. RemoveInput).
//
// ctx must carry a deadline or timeout: SetSpeaking sends a gateway op and will
// block until Discord acknowledges or the context is cancelled.
func (v *VoiceConnSetup) Apply(ctx context.Context, conn voice.Conn, chIn <-chan []byte) (chan []byte, *opus.FanoutHandle, func(), error) {
	var provider voice.OpusFrameProvider
	if v.providerFn == nil {
		provider = opus.NewEmptyVoiceProvider()
	} else {
		p, err := v.providerFn(chIn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("create voice provider: %w", err)
		}
		provider = p
	}

	var capture chan []byte
	var receiver voice.OpusFrameReceiver
	var handle *opus.FanoutHandle
	if v.receiverFn == nil {
		receiver = opus.NewEmptyVoiceReceiver()
	} else {
		ch, r, h, err := v.receiverFn()
		if err != nil {
			provider.Close()
			return nil, nil, nil, fmt.Errorf("create voice receiver: %w", err)
		}
		receiver = r
		capture = ch
		handle = h
	}

	cleanup := func() {
		provider.Close()
		receiver.Close()
		if handle != nil {
			handle.Close()
		}
		if capture != nil {
			close(capture)
		}
	}

	conn.SetOpusFrameProvider(provider)
	conn.SetOpusFrameReceiver(receiver)

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("set speaking flag: %w", err)
	}

	return capture, handle, cleanup, nil
}
