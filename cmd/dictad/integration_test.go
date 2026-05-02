package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/dicta/internal/audio"
)

// TestEndToEnd_AudioToASR drives the full phase-3+phase-4 path: a
// stub-PATH pw-record produces silence-then-speech-then-silence PCM,
// the audioMonitor's VAD detects an utterance, the asrMonitor's
// OnUtterance callback fires, the fake backend returns a transcript,
// and asrMonitor's Snapshot reflects the result.
func TestEndToEnd_AudioToASR(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "clip.pcm")
	if err := writeUtterancePCM(pcmPath); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "pw-record")
	// Pace stdout at ~80 ms / 2560-byte chunks so the audioMonitor's
	// channel buffer doesn't overflow and drop frames. A real
	// pw-record paces output at sample rate; bare `cat` would dump
	// instantly and the non-blocking pump would discard most frames.
	script := "#!/bin/sh\n" +
		"exec dd if=" + pcmPath + " bs=2560 2>/dev/null | while dd bs=2560 count=1 2>/dev/null; do sleep 0.05; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	fake := &fakeASR{transcript: asrclient.Transcript{Text: "captured speech", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), fake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})

	audioMon := newAudioMonitor(discardLogger(),
		audio.CaptureConfig{Backend: audio.BackendPipeWire},
		audio.VADConfig{Calibrate: 100 * time.Millisecond})
	audioMon.onUtterance = asrMon.OnUtterance

	if err := audioMon.Start(t.Context()); err != nil {
		t.Fatalf("audio.Start: %v", err)
	}
	defer audioMon.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if asrMon.Snapshot().Transcripts >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	asrSnap := asrMon.Snapshot()
	audioSnap := audioMon.Snapshot()
	if asrSnap.Transcripts == 0 {
		t.Fatalf("expected at least 1 transcript; audio=%+v asr=%+v", audioSnap, asrSnap)
	}
	if asrSnap.LastTranscript != "captured speech" {
		t.Errorf("LastTranscript: got %q want %q", asrSnap.LastTranscript, "captured speech")
	}
	if audioSnap.SpeechFrames == 0 {
		t.Errorf("audio.SpeechFrames: got 0; expected VAD detection")
	}
}

// writeUtterancePCM writes 300 ms silence + 1000 ms 440 Hz tone +
// 1500 ms silence. The trailing silence is needed for the VAD's 800 ms
// hangover to fire end-of-utterance, which is what triggers OnUtterance.
func writeUtterancePCM(path string) error {
	silenceLead := audio.SampleRateHz * 300 / 1000
	tone := audio.SampleRateHz * 1000 / 1000
	silenceTrail := audio.SampleRateHz * 1500 / 1000
	total := silenceLead + tone + silenceTrail
	buf := make([]byte, total*audio.SampleWidth)
	for i := silenceLead; i < silenceLead+tone; i++ {
		v := 0.6 * math.Sin(2*math.Pi*440*float64(i-silenceLead)/audio.SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return os.WriteFile(path, buf, 0o644)
}
