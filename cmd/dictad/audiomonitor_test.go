package main

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/audio"
)

// TestAudioMonitor_DetectsSpeech wires a stub pw-record that streams
// 1 second of loud sine-wave PCM, then verifies the monitor's
// AudioStats.SpeechFrames increment and last_vad_state flips to "speech".
//
// This validates the phase-3 "audio frames flowing and VAD transitions"
// deliverable end-to-end at the daemon layer: capture subprocess →
// io.ReadFull pump → ring buffer push → VAD classify → atomic stats.
func TestAudioMonitor_DetectsSpeech(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "loud.pcm")
	if err := writeLoudPCM(pcmPath); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "pw-record")
	script := "#!/bin/sh\nexec cat " + pcmPath + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := newAudioMonitor(logger, audio.CaptureConfig{Backend: audio.BackendPipeWire}, audio.VADConfig{
		Calibrate: 100 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := mon.Snapshot()
		if s.SpeechFrames > 0 && s.LastVADState == "speech" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := mon.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}

	final := mon.Snapshot()
	if final.Frames < 5 {
		t.Errorf("Frames: got %d want at least 5", final.Frames)
	}
	if final.SpeechFrames == 0 {
		t.Errorf("SpeechFrames: got 0; expected VAD to detect loud sine wave (final=%+v)", final)
	}
	if final.Backend != "pipewire" {
		t.Errorf("Backend: got %q want pipewire", final.Backend)
	}
}

func TestAudioMonitor_StopWithoutStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := newAudioMonitor(logger, audio.CaptureConfig{}, audio.VADConfig{})
	if err := mon.Stop(); err != nil {
		t.Errorf("Stop on unstarted monitor: %v", err)
	}
}

// TestAudioMonitor_MaxUtteranceCap_ForceSplits drives a long
// continuous tone through the capture pipeline and asserts that the
// MaxUtterance cap force-emits in bounded chunks instead of letting
// one accumulator grow without limit.
//
// Regression test for the field bug where the VAD never declared
// end-of-utterance for 112 seconds and dispatched a single bundled
// transcript ("Testing. Testing. Testing.") to ydotool.
func TestAudioMonitor_MaxUtteranceCap_ForceSplits(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "long.pcm")
	// 5 seconds of continuous 440 Hz tone — well above the 1-second cap
	// the test sets, so the loop must force-emit at least 5 times.
	const seconds = 5
	if err := writeContinuousTonePCM(pcmPath, seconds); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "pw-record")
	// Pace ~80 ms per chunk so the VAD sees real frame timing rather
	// than a flood-then-EOF pattern.
	script := "#!/bin/sh\n" +
		"exec dd if=" + pcmPath + " bs=2560 2>/dev/null | while dd bs=2560 count=1 2>/dev/null; do sleep 0.05; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := newAudioMonitor(logger,
		audio.CaptureConfig{Backend: audio.BackendPipeWire},
		audio.VADConfig{
			// Short calibration so the test's leading silence is enough.
			Calibrate: 100 * time.Millisecond,
			// Long hangover so end-of-utterance never naturally fires
			// during the 5-second tone — only the cap should force a
			// split.
			Hangover: 30 * time.Second,
		})

	// Cap at 1 second of audio = 32000 bytes (16 kHz * 2 bytes/sample).
	const capBytes = audio.SampleRateHz * audio.SampleWidth * 1
	mon.SetMaxUtterance(capBytes)

	var (
		emissions [][]byte
		emitMu    sync.Mutex
	)
	mon.onUtterance = func(pcm []byte) {
		emitMu.Lock()
		defer emitMu.Unlock()
		// Copy into our slice; mon hands off a fresh allocation each time.
		captured := make([]byte, len(pcm))
		copy(captured, pcm)
		emissions = append(emissions, captured)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait long enough for the 5-second tone to stream through.
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		emitMu.Lock()
		n := len(emissions)
		emitMu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := mon.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}

	emitMu.Lock()
	defer emitMu.Unlock()
	if len(emissions) < 4 {
		t.Fatalf("expected ≥4 force-emitted chunks during 5s of tone with 1s cap; got %d", len(emissions))
	}
	// Every emission must respect the cap. Allow one frame's worth of
	// overshoot (2560 bytes) since the cap check happens after append.
	for i, em := range emissions {
		if len(em) > capBytes+2560 {
			t.Errorf("emission %d: size %d > cap %d + frame slack", i, len(em), capBytes)
		}
	}
}

// writeContinuousTonePCM writes 100 ms of silence followed by `seconds`
// seconds of 440 Hz tone. The leading silence lets the VAD calibrate.
func writeContinuousTonePCM(path string, seconds int) error {
	silenceSamples := audio.SampleRateHz * 100 / 1000
	toneSamples := audio.SampleRateHz * seconds
	buf := make([]byte, (silenceSamples+toneSamples)*audio.SampleWidth)
	for i := silenceSamples; i < silenceSamples+toneSamples; i++ {
		v := 0.6 * math.Sin(2*math.Pi*440*float64(i-silenceSamples)/audio.SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return os.WriteFile(path, buf, 0o644)
}

// writeLoudPCM writes 300 ms of digital silence followed by 1500 ms of a
// 440 Hz tone at 0.6 amplitude. The leading silence lets the VAD's
// 100 ms calibration window settle on an actual silence floor (rather
// than the test's loud signal) before classification begins.
func writeLoudPCM(path string) error {
	silenceSamples := audio.SampleRateHz * 300 / 1000
	toneSamples := audio.SampleRateHz * 1500 / 1000
	buf := make([]byte, (silenceSamples+toneSamples)*audio.SampleWidth)
	for i := silenceSamples; i < silenceSamples+toneSamples; i++ {
		v := 0.6 * math.Sin(2*math.Pi*440*float64(i-silenceSamples)/audio.SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return os.WriteFile(path, buf, 0o644)
}
