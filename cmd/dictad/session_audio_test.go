package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/dicta/internal/audio"
)

// fakeCaptureLifecycle stands in for audioMonitor's on-demand lifecycle
// hooks. Start and Stop sleep briefly so concurrent toggles interleave.
type fakeCaptureLifecycle struct {
	mu       sync.Mutex
	running  bool
	starts   int
	stops    int
	startErr error
}

func (f *fakeCaptureLifecycle) Start(context.Context) error {
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	if !f.running {
		f.starts++
		f.running = true
	}
	return nil
}

func (f *fakeCaptureLifecycle) Stop() error {
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		f.stops++
		f.running = false
	}
	return nil
}

func (f *fakeCaptureLifecycle) State() (running bool, starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, f.starts, f.stops
}

func TestSession_OnDemandCapture_RunsOnlyWhileOpen(t *testing.T) {
	s, _, _, _, _ := newTestSession(t)
	capture := &fakeCaptureLifecycle{}
	s.SetAudioLifecycle(capture.Start, capture.Stop)

	if running, _, _ := capture.State(); running {
		t.Fatal("capture running before any session opened")
	}
	for i := 1; i <= 2; i++ {
		if err := s.Toggle(t.Context(), "type"); err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if running, starts, _ := capture.State(); !running || starts != i {
			t.Errorf("after open #%d: running=%v starts=%d", i, running, starts)
		}
		if err := s.Toggle(t.Context(), "type"); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
		if running, _, stops := capture.State(); running || stops != i {
			t.Errorf("after close #%d: running=%v stops=%d", i, running, stops)
		}
	}
}

func TestSession_OnDemandCapture_StartFailureRollsBack(t *testing.T) {
	// A microphone that cannot be opened must not leave a session that
	// looks open but can never hear anything, and must not play the cue.
	s, _, cuer, _, _ := newTestSession(t)
	capture := &fakeCaptureLifecycle{startErr: errors.New("pw-record: no such device")}
	s.SetAudioLifecycle(capture.Start, capture.Stop)

	if err := s.Toggle(t.Context(), "type"); err == nil {
		t.Fatal("expected capture start error")
	}
	if _, open := s.Snapshot(); open {
		t.Error("session should be closed after capture failed to start")
	}
	if played := cuer.Played(); len(played) != 0 {
		t.Errorf("no cue should play for a failed open; got %v", played)
	}
}

func TestSession_OnDemandCapture_ClipSpawnFailureStopsCapture(t *testing.T) {
	s, _, preview, _ := newClipSession(t)
	capture := &fakeCaptureLifecycle{}
	s.SetAudioLifecycle(capture.Start, capture.Stop)
	preview.spawnErr = errors.New("exec format error")

	if err := s.Toggle(t.Context(), "clip"); err == nil {
		t.Fatal("expected spawn error")
	}
	if running, starts, _ := capture.State(); running || starts != 1 {
		t.Errorf("capture should have started then stopped; running=%v starts=%d", running, starts)
	}
}

func TestSession_OnDemandCapture_ModeSwitchKeepsCapturing(t *testing.T) {
	// D6: opening type while clip is open closes clip first. Capture
	// restarts for the new session rather than staying off.
	s, _, _, _ := newClipSession(t)
	capture := &fakeCaptureLifecycle{}
	s.SetAudioLifecycle(capture.Start, capture.Stop)

	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	if mode, open := s.Snapshot(); mode != "type" || !open {
		t.Fatalf("got mode=%q open=%v want type/true", mode, open)
	}
	if running, _, _ := capture.State(); !running {
		t.Error("capture should be running for the new type session")
	}
}

func TestSession_OnDemandCapture_ShutdownStopsCapture(t *testing.T) {
	s, _, _, _, _ := newTestSession(t)
	capture := &fakeCaptureLifecycle{}
	s.SetAudioLifecycle(capture.Start, capture.Stop)

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if running, _, _ := capture.State(); running {
		t.Error("capture still running after shutdown")
	}
}

func TestSession_OnDemandCapture_VADResetBeforeCaptureStarts(t *testing.T) {
	// Reset must not race a live audio loop, so it has to happen while
	// on-demand capture is still stopped.
	typer := &fakeTyper{}
	capture := &fakeCaptureLifecycle{}
	vad := &captureCheckingVAD{capture: capture}
	asrMon := newASRMonitor(discardLogger(), &fakeASR{}, asrMonitorConfig{BackendName: "fake"})
	s := newSession(discardLogger(), typer, nil, &fakeCuer{}, asrMon, vad, nil, nil, nil, nil, nil, t.Context())
	s.SetAudioLifecycle(capture.Start, capture.Stop)

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	vad.mu.Lock()
	defer vad.mu.Unlock()
	if vad.resets != 1 || vad.resetWhileRunning {
		t.Errorf("resets=%d resetWhileRunning=%v; want 1 reset with capture stopped", vad.resets, vad.resetWhileRunning)
	}
}

type captureCheckingVAD struct {
	capture           *fakeCaptureLifecycle
	mu                sync.Mutex
	resets            int
	resetWhileRunning bool
}

func (v *captureCheckingVAD) IsSpeech(audio.Frame) bool { return false }

func (v *captureCheckingVAD) Reset() {
	running, _, _ := v.capture.State()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resets++
	if running {
		v.resetWhileRunning = true
	}
}

func TestSession_OnDemandCapture_ConcurrentTogglesConverge(t *testing.T) {
	// Toggles are not serialised, so a close and an open can overlap.
	// Whatever order they land in, capture must end up matching the
	// final session state -- never a closed session still capturing,
	// never an open one that is deaf.
	for round := range 20 {
		s, _, _, _, _ := newTestSession(t)
		capture := &fakeCaptureLifecycle{}
		s.SetAudioLifecycle(capture.Start, capture.Stop)

		var wg sync.WaitGroup
		for range 7 {
			wg.Go(func() { _ = s.Toggle(t.Context(), "type") })
		}
		wg.Wait()

		_, open := s.Snapshot()
		if running, _, _ := capture.State(); running != open {
			t.Fatalf("round %d: session open=%v but capture running=%v", round, open, running)
		}
	}
}

// TestEndToEnd_OnDemandCapture runs the real audioMonitor under the
// session's on-demand lifecycle: capture is off until the session opens,
// the utterance is typed, capture stops on close, and restarts on reopen.
func TestEndToEnd_OnDemandCapture(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "clip.pcm")
	if err := writeUtterancePCM(pcmPath); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "pw-record")
	script := "#!/bin/sh\n" +
		"exec dd if=" + pcmPath + " bs=2560 2>/dev/null | while dd bs=2560 count=1 2>/dev/null; do sleep 0.05; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	fake := &fakeASR{transcript: asrclient.Transcript{Text: "captured speech", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), fake, asrMonitorConfig{
		BackendName:       "fake",
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	audioMon := newAudioMonitor(discardLogger(),
		audio.CaptureConfig{Backend: audio.BackendPipeWire},
		audio.VADConfig{Calibrate: 100 * time.Millisecond})
	defer func() { _ = audioMon.Stop() }()

	typer := &fakeTyper{}
	sess := newSession(discardLogger(), typer, nil, &fakeCuer{}, asrMon, audioMon.VAD(), nil, nil, nil, nil, audioMon.Flush, t.Context())
	audioMon.onUtterance = sess.OnUtterance
	sess.SetAudioLifecycle(audioMon.StartIfStopped, audioMon.Stop)

	if audioMon.Snapshot().Running {
		t.Fatal("capture running before the session opened")
	}
	if err := sess.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle open: %v", err)
	}
	if !audioMon.Snapshot().Running {
		t.Fatal("capture not running after the session opened")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(typer.Calls()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if calls := typer.Calls(); len(calls) == 0 || calls[0] != "captured speech" {
		t.Fatalf("type calls: got %v want [captured speech]; audio=%+v asr=%+v",
			calls, audioMon.Snapshot(), asrMon.Snapshot())
	}

	if err := sess.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle close: %v", err)
	}
	if audioMon.Snapshot().Running {
		t.Error("capture still running after the session closed")
	}

	if err := sess.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle reopen: %v", err)
	}
	if !audioMon.Snapshot().Running {
		t.Error("capture did not restart when the session reopened")
	}
	if err := sess.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle close again: %v", err)
	}
}
