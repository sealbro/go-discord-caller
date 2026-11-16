package opus

import (
	"math"
	"testing"

	"github.com/sealbro/go-discord-caller/internal/telemetry"
)

func TestMixLoudSourcesClampInsteadOfWrapping(t *testing.T) {
	m, err := NewMixer(telemetry.OpusRecorder{})
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}

	const sources = 6
	for range sources {
		pcm := GetPCM()
		for i := range pcm {
			pcm[i] = math.MaxInt16
		}
		m.framesBuf = append(m.framesBuf, Frame{PCM: pcm})
	}

	out, err := m.mixAndEncode()
	if err != nil {
		t.Fatalf("mixAndEncode: %v", err)
	}
	PutEncodedFrame(out)

	for i, v := range m.pcm {
		if v != math.MaxInt16 {
			t.Fatalf("sample %d = %d, want %d: summing %d full-scale sources wrapped instead of clamping", i, v, math.MaxInt16, sources)
		}
	}
}
