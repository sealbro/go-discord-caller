package test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"

	"github.com/disgoorg/disgo/voice"
)

// dcaEntry holds an open .dca file with its frame start offset.
type dcaEntry struct {
	path      string
	file      *os.File
	frameBase int64
}

// RandomFileVoiceProvider streams Opus frames from a single .dca file chosen
// randomly at construction time, looping it indefinitely.
type RandomFileVoiceProvider struct {
	voice.OpusFrameProvider
	entry *dcaEntry
	done  chan struct{}
}

// NewRandomFileVoiceProvider picks one file at random from paths, opens it,
// and returns a provider that loops that file indefinitely. Paths must not be empty.
func NewRandomFileVoiceProvider(paths []string) (*RandomFileVoiceProvider, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dca files provided")
	}
	p := paths[rand.IntN(len(paths))]
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open dca file %q: %w", p, err)
	}
	frameBase, err := skipDCAHeader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("parse dca header %q: %w", p, err)
	}
	return &RandomFileVoiceProvider{
		entry: &dcaEntry{path: p, file: f, frameBase: frameBase},
		done:  make(chan struct{}),
	}, nil
}

func (v *RandomFileVoiceProvider) ProvideOpusFrame() ([]byte, error) {
	select {
	case <-v.done:
		return nil, fmt.Errorf("random voice provider is closed")
	default:
	}

	for {
		frame, err := readDCAFrame(v.entry.file)
		if err == nil {
			return frame, nil
		}
		if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("read dca frame %q: %w", v.entry.path, err)
		}
		// EOF — loop the file from the start of its frame data.
		slog.Debug("dca loop: restarting file", slog.String("path", v.entry.path))
		if _, seekErr := v.entry.file.Seek(v.entry.frameBase, io.SeekStart); seekErr != nil {
			return nil, fmt.Errorf("seek dca file %q: %w", v.entry.path, seekErr)
		}
	}
}

func (v *RandomFileVoiceProvider) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
		_ = v.entry.file.Close()
	}
}

// skipDCAHeader advances f past the optional DCA1 header and returns the
// byte offset at which frame data begins.
func skipDCAHeader(f *os.File) (int64, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return 0, err
	}

	if string(magic) != "DCA1" {
		// No header — rewind; frames start at byte 0.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Read int32-LE JSON metadata length.
	var jsonLen int32
	if err := binary.Read(f, binary.LittleEndian, &jsonLen); err != nil {
		return 0, err
	}

	// Skip JSON metadata.
	meta := make([]byte, jsonLen)
	if _, err := io.ReadFull(f, meta); err != nil {
		return 0, err
	}

	// Validate JSON (best-effort).
	var dummy any
	if err := json.Unmarshal(meta, &dummy); err != nil {
		slog.Warn("dca header JSON invalid, ignoring", slog.Any("err", err))
	}

	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return pos, nil
}

// readDCAFrame reads one int16-LE-prefixed Opus frame from f.
func readDCAFrame(f *os.File) ([]byte, error) {
	var frameSize int16
	if err := binary.Read(f, binary.LittleEndian, &frameSize); err != nil {
		return nil, err
	}
	if frameSize <= 0 {
		return nil, fmt.Errorf("invalid frame size: %d", frameSize)
	}

	buf := make([]byte, frameSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
