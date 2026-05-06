package e2e

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

// FileVoiceProvider streams Opus frames from a .dca file, looping infinitely.
// DCA format: optional "DCA1" magic + int32-LE JSON length + JSON metadata,
// then repeated frames: int16-LE frame size + raw Opus bytes.
type FileVoiceProvider struct {
	voice.OpusFrameProvider
	path      string
	file      *os.File
	frameBase int64 // byte offset where frames begin (after header)
	done      chan struct{}
}

func NewFileVoiceProvider(path string) (*FileVoiceProvider, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dca file: %w", err)
	}

	frameBase, err := skipDCAHeader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("parse dca header: %w", err)
	}

	return &FileVoiceProvider{
		path:      path,
		file:      f,
		frameBase: frameBase,
		done:      make(chan struct{}),
	}, nil
}

func (v *FileVoiceProvider) ProvideOpusFrame() ([]byte, error) {
	select {
	case <-v.done:
		return nil, fmt.Errorf("file voice provider is closed")
	default:
	}

	for {
		frame, err := readDCAFrame(v.file)
		if err == nil {
			return frame, nil
		}

		if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("read dca frame: %w", err)
		}

		// Loop: seek back to the first frame.
		slog.Debug("dca file looping", slog.String("path", v.path))
		if _, seekErr := v.file.Seek(v.frameBase, io.SeekStart); seekErr != nil {
			return nil, fmt.Errorf("seek dca file: %w", seekErr)
		}
	}
}

func (v *FileVoiceProvider) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
		_ = v.file.Close()
	}
}

// dcaEntry holds an open .dca file with its frame start offset.
type dcaEntry struct {
	path      string
	file      *os.File
	frameBase int64
}

// RandomFileVoiceProvider streams Opus frames from multiple .dca files,
// picking a new file at random each time the current one reaches EOF.
// When only one file is provided it loops that file indefinitely.
type RandomFileVoiceProvider struct {
	voice.OpusFrameProvider
	entries    []*dcaEntry
	currentIdx int
	done       chan struct{}
}

// NewRandomFileVoiceProvider opens all paths and returns a provider that
// plays them in random order. Paths must not be empty.
func NewRandomFileVoiceProvider(paths []string) (*RandomFileVoiceProvider, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dca files provided")
	}
	entries := make([]*dcaEntry, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			for _, e := range entries {
				_ = e.file.Close()
			}
			return nil, fmt.Errorf("open dca file %q: %w", p, err)
		}
		frameBase, err := skipDCAHeader(f)
		if err != nil {
			_ = f.Close()
			for _, e := range entries {
				_ = e.file.Close()
			}
			return nil, fmt.Errorf("parse dca header %q: %w", p, err)
		}
		entries = append(entries, &dcaEntry{path: p, file: f, frameBase: frameBase})
	}
	return &RandomFileVoiceProvider{
		entries:    entries,
		currentIdx: rand.IntN(len(entries)),
		done:       make(chan struct{}),
	}, nil
}

func (v *RandomFileVoiceProvider) ProvideOpusFrame() ([]byte, error) {
	select {
	case <-v.done:
		return nil, fmt.Errorf("random voice provider is closed")
	default:
	}

	for {
		cur := v.entries[v.currentIdx]
		frame, err := readDCAFrame(cur.file)
		if err == nil {
			return frame, nil
		}

		if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("read dca frame %q: %w", cur.path, err)
		}

		// Pick a random next file. With more than one file, avoid replaying
		// the same file back-to-back by excluding the current index.
		next := v.currentIdx
		if len(v.entries) > 1 {
			next = rand.IntN(len(v.entries) - 1)
			if next >= v.currentIdx {
				next++
			}
		}
		v.currentIdx = next
		nextEntry := v.entries[v.currentIdx]
		slog.Debug("dca random: next file", slog.String("path", nextEntry.path))
		if _, seekErr := nextEntry.file.Seek(nextEntry.frameBase, io.SeekStart); seekErr != nil {
			return nil, fmt.Errorf("seek dca file %q: %w", nextEntry.path, seekErr)
		}
	}
}

func (v *RandomFileVoiceProvider) Close() {
	select {
	case <-v.done:
	default:
		close(v.done)
		for _, e := range v.entries {
			_ = e.file.Close()
		}
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
