// gen-samples generates DCA audio files under integration/samples/ used by the
// integration test harness as the source-bot's audio stream.
//
// On macOS it uses the built-in `say` command to produce one file per speaker
// voice (Alex, Samantha, Victoria, Karen, Daniel), each saying
// "Hello this is <Name>". It also writes a generic test.dca as a fallback.
//
// Pipeline:
//
//	say / espeak-ng → AIFF/WAV → ffmpeg → raw s16le PCM → hraban/opus encoder → DCA
//
// Requires: ffmpeg in PATH. TTS: say (macOS) or espeak-ng (Linux/other).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	hraban "github.com/hraban/opus"
)

const (
	sampleRate      = 48000
	channels        = 2
	frameDurationMs = 20
	samplesPerFrame = sampleRate / 1000 * frameDurationMs // 960 samples per channel
	pcmFrameLen     = samplesPerFrame * channels          // 1920 int16 values per frame
	opusBufSize     = 4000                                // max encoded bytes per 20 ms frame
)

// macSpeakers lists the macOS say voices and the text each one speaks.
// Each speaker says their own name so they are identifiable during integration tests.
var macSpeakers = []struct {
	voice string
	name  string
}{
	{"Alex", "Alex"},
	{"Samantha", "Samantha"},
	{"Victoria", "Victoria"},
	{"Karen", "Karen"},
	{"Daniel", "Daniel"},
}

// espeakSpeakers lists espeak-ng voices used on non-macOS platforms.
var espeakSpeakers = []struct {
	voice string
	name  string
}{
	{"en", "English"},
	{"en-us", "American"},
	{"en-gb", "British"},
}

func main() {
	outDir := flag.String("outdir", "integration/samples", "output directory for .dca files")
	bitrate := flag.Int("bitrate", 64000, "Opus bitrate in bps")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}

	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("say"); err == nil {
			generateWithSay(*outDir, *bitrate)
			return
		}
	}

	if _, err := exec.LookPath("espeak-ng"); err == nil {
		generateWithEspeak(*outDir, *bitrate)
		return
	}

	log.Fatal("no TTS tool found — install espeak-ng (Linux) or run on macOS (say)")
}

func generateWithSay(outDir string, bitrate int) {
	for _, sp := range macSpeakers {
		text := fmt.Sprintf("Hello this is %s", sp.name)
		outFile := filepath.Join(outDir, strings.ToLower(sp.name)+".dca")
		if err := generate(text, sp.voice, outFile, bitrate); err != nil {
			log.Fatalf("[%s] %v", sp.voice, err)
		}
	}
}

func generateWithEspeak(outDir string, bitrate int) {
	for _, sp := range espeakSpeakers {
		text := fmt.Sprintf("Hello this is %s", sp.name)
		outFile := filepath.Join(outDir, strings.ToLower(sp.name)+".dca")
		if err := generate(text, sp.voice, outFile, bitrate); err != nil {
			log.Fatalf("[%s] %v", sp.voice, err)
		}
	}
}

func generate(text, voice, outFile string, bitrate int) error {
	wavFile, err := synthesize(text, voice)
	if err != nil {
		return fmt.Errorf("TTS: %w", err)
	}
	defer os.Remove(wavFile)

	pcmFile, err := toPCM(wavFile)
	if err != nil {
		return fmt.Errorf("PCM: %w", err)
	}
	defer os.Remove(pcmFile)

	n, err := encodeDCA(pcmFile, outFile, bitrate)
	if err != nil {
		return fmt.Errorf("DCA: %w", err)
	}
	log.Printf("%-12s → %s (%d frames, %.1f s)", voice, outFile, n, float64(n)*float64(frameDurationMs)/1000)
	return nil
}

// synthesize runs espeak-ng or macOS say and returns the path to a temporary audio file.
// voice is the espeak-ng voice name (e.g. "en") or the macOS say voice name (e.g. "Alex").
func synthesize(text, voice string) (string, error) {
	f, err := os.CreateTemp("", "gen-samples-*.wav")
	if err != nil {
		return "", err
	}
	wavPath := f.Name()
	f.Close()

	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("say"); err == nil {
			// Output AIFF; ffmpeg resamples to 48 kHz stereo in the next step.
			aiffPath := wavPath + ".aiff"
			out, err := exec.Command(path,
				"-v", voice,
				"-r", "150",
				text,
				"-o", aiffPath,
			).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("say -v %s failed: %v\n%s", voice, err, out)
			}
			_ = os.Remove(wavPath)
			return aiffPath, nil
		}
	}

	if path, err := exec.LookPath("espeak-ng"); err == nil {
		out, err := exec.Command(path,
			"-v", voice,
			"-s", "150",
			text,
			"-w", wavPath,
		).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("espeak-ng -v %s failed: %v\n%s", voice, err, out)
		}
		return wavPath, nil
	}

	return "", fmt.Errorf("no TTS tool found — install espeak-ng or run on macOS (say)")
}

// toPCM converts any audio file to raw signed 16-bit little-endian PCM at
// 48 kHz stereo using ffmpeg. Returns the path to a temporary PCM file.
func toPCM(audioFile string) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	f, err := os.CreateTemp("", "gen-samples-*.pcm")
	if err != nil {
		return "", err
	}
	pcmPath := f.Name()
	f.Close()

	out, err := exec.Command(ffmpeg,
		"-y",
		"-i", audioFile,
		"-f", "s16le",
		"-ar", fmt.Sprint(sampleRate),
		"-ac", fmt.Sprint(channels),
		pcmPath,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %v\n%s", err, out)
	}
	return pcmPath, nil
}

// encodeDCA reads raw s16le PCM, encodes it to Opus at 20 ms frames, and writes
// a DCA file (no header; each frame is int16-LE size followed by Opus bytes).
// Returns the number of frames written.
func encodeDCA(pcmFile, outFile string, bitrate int) (int, error) {
	enc, err := hraban.NewEncoder(sampleRate, channels, hraban.AppVoIP)
	if err != nil {
		return 0, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(bitrate); err != nil {
		return 0, fmt.Errorf("set bitrate: %w", err)
	}

	raw, err := os.ReadFile(pcmFile)
	if err != nil {
		return 0, fmt.Errorf("read pcm: %w", err)
	}

	// Convert raw bytes to int16 samples.
	samples := make([]int16, len(raw)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}

	dst, err := os.Create(outFile)
	if err != nil {
		return 0, fmt.Errorf("create output: %w", err)
	}
	defer dst.Close()

	opusBuf := make([]byte, opusBufSize)
	frames := 0

	for offset := 0; offset+pcmFrameLen <= len(samples); offset += pcmFrameLen {
		n, err := enc.Encode(samples[offset:offset+pcmFrameLen], opusBuf)
		if err != nil {
			return frames, fmt.Errorf("encode frame %d: %w", frames, err)
		}
		if err := binary.Write(dst, binary.LittleEndian, int16(n)); err != nil {
			return frames, err
		}
		if _, err := dst.Write(opusBuf[:n]); err != nil {
			return frames, err
		}
		frames++
	}

	return frames, nil
}
