package manager

import (
	"context"
	"fmt"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/opus"
)

// VoiceConnSetup is a builder that configures a voice connection's provider
// and receiver pair. Use the With* methods to select the desired mode, then
// call Apply to wire everything into a voice.Conn.
type VoiceConnSetup struct {
	userID     snowflake.ID
	providerFn func(chIn <-chan []byte) (voice.OpusFrameProvider, error)
	receiverFn func() (chan []byte, voice.OpusFrameReceiver, error)
	captureCh  chan []byte // set by WithTeeFileProvider
}

// NewVoiceConnSetup creates a new voice session builder.
func NewVoiceConnSetup(userID snowflake.ID) *VoiceConnSetup {
	return &VoiceConnSetup{userID: userID}
}

// WithFileProvider plays audio from a DCA file, draining chIn.
func (v *VoiceConnSetup) WithFileProvider(path string) *VoiceConnSetup {
	v.providerFn = func(chIn <-chan []byte) (voice.OpusFrameProvider, error) {
		if chIn != nil {
			go func() {
				for range chIn {
				}
			}()
		}
		return opus.NewFileVoiceProvider(path)
	}
	return v
}

// WithTeeFileProvider plays audio from a DCA file, drains chIn, and tees
// the file frames into a capture channel returned by Apply.
func (v *VoiceConnSetup) WithTeeFileProvider(path string) *VoiceConnSetup {
	v.captureCh = make(chan []byte, audioChanBuf)
	tee := v.captureCh
	v.providerFn = func(chIn <-chan []byte) (voice.OpusFrameProvider, error) {
		if chIn != nil {
			go func() {
				for range chIn {
				}
			}()
		}
		fp, err := opus.NewFileVoiceProvider(path)
		if err != nil {
			return nil, err
		}
		return opus.NewTeeProvider(fp, tee), nil
	}
	return v
}

// WithVoiceProvider reads opus frames from chIn and plays them.
func (v *VoiceConnSetup) WithVoiceProvider() *VoiceConnSetup {
	v.providerFn = func(chIn <-chan []byte) (voice.OpusFrameProvider, error) {
		return opus.NewVoiceProvider(chIn), nil
	}
	return v
}

// WithVoiceReceiver captures incoming voice frames filtered by allowUser.
func (v *VoiceConnSetup) WithVoiceReceiver(allowUser func(snowflake.ID) bool) *VoiceConnSetup {
	v.receiverFn = func() (chan []byte, voice.OpusFrameReceiver, error) {
		ch := make(chan []byte, audioChanBuf)
		return ch, opus.NewVoiceReceiver(ch, v.userID, allowUser), nil
	}
	return v
}

// Apply configures the voice connection with the session's provider and receiver,
// sets the speaking flag, and returns the capture output channel (nil when no
// capture is configured) together with a cleanup function.
func (v *VoiceConnSetup) Apply(ctx context.Context, conn voice.Conn, chIn <-chan []byte) (chan []byte, func(), error) {
	var provider voice.OpusFrameProvider
	if v.providerFn == nil {
		provider = opus.NewEmptyVoiceProvider()
	} else {
		p, err := v.providerFn(chIn)
		if err != nil {
			return nil, nil, fmt.Errorf("create voice provider: %w", err)
		}
		provider = p
	}

	var capture chan []byte
	var receiver voice.OpusFrameReceiver
	if v.receiverFn == nil {
		receiver = opus.NewEmptyVoiceReceiver()
		capture = v.captureCh
	} else {
		ch, r, err := v.receiverFn()
		if err != nil {
			return nil, nil, fmt.Errorf("create voice receiver: %w", err)
		}
		receiver = r
		capture = ch
	}

	conn.SetOpusFrameProvider(provider)
	conn.SetOpusFrameReceiver(receiver)

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		return nil, nil, fmt.Errorf("set speaking flag: %w", err)
	}

	return capture, func() {
		provider.Close()
		receiver.Close()
		if capture != nil {
			close(capture)
		}
	}, nil
}
