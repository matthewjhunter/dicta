package main

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
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
