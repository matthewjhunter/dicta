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
	"sync/atomic"
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

// TestAudioMonitor_MinSpeechFramesGate_DropsBlip is the regression test
// for the "Thank you" hallucination on session-open. A single frame of
// loud audio followed by 800 ms of hangover silence used to be emitted
// as an ~880 ms utterance, which Whisper-family backends transcribed as
// "Thank you" / "Thanks for watching" / "you". With the gate set to
// require ≥3 raw-speech frames, the blip must be dropped before
// reaching ASR; a longer real utterance must still pass through.
func TestAudioMonitor_MinSpeechFramesGate_DropsBlip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := newFakeCapture()
	mon := &audioMonitor{
		cap: fc,
		// Short calibration + short hangover so the test stays compact;
		// the gate logic doesn't depend on either being default.
		vad: audio.NewEnergyVAD(audio.VADConfig{
			Calibrate: 80 * time.Millisecond,
			Hangover:  240 * time.Millisecond,
		}),
		rb:  audio.NewRingBuffer(audio.CapacityForSeconds(5)),
		log: logger,
	}
	mon.SetMinRawSpeechFrames(3)
	mon.backend.Store("")

	var (
		emissions [][]byte
		emitMu    sync.Mutex
	)
	mon.onUtterance = func(pcm []byte) {
		emitMu.Lock()
		defer emitMu.Unlock()
		captured := make([]byte, len(pcm))
		copy(captured, pcm)
		emissions = append(emissions, captured)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	loud := loudFrame(0.6)
	silent := make([]byte, audio.FrameBytes)

	// Phase 1: calibration + 1-frame blip (this is what hallucinates).
	fc.send(silent)
	fc.send(silent) // 160 ms — beyond the 80 ms calibration
	fc.send(loud)   // single raw-speech frame
	// Hangover-window silence: VAD reports speech under hangover, raw
	// stays false. After 240 ms (3 frames) hangover expires.
	for range 4 {
		fc.send(silent)
	}

	// Phase 2: 5 raw-speech frames in a row — clearly past the gate.
	for range 5 {
		fc.send(loud)
	}
	for range 4 {
		fc.send(silent)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		emitMu.Lock()
		n := len(emissions)
		emitMu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := mon.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}

	emitMu.Lock()
	defer emitMu.Unlock()
	if len(emissions) != 1 {
		t.Fatalf("expected exactly 1 emission (blip dropped, real speech kept); got %d", len(emissions))
	}
	// Sanity: the surviving emission must be the longer one. 5 raw +
	// 3 hangover frames = 8 frames * FrameBytes minimum; allow some
	// slack for scheduling-induced extras.
	minBytes := 5 * audio.FrameBytes
	if len(emissions[0]) < minBytes {
		t.Errorf("surviving emission length %d < expected real-speech min %d",
			len(emissions[0]), minBytes)
	}
}

// TestAudioMonitor_FlushEmitsInFlightUtterance is the regression test
// for the "tapped mute too fast and lost my last sentence" bug. With
// a long VAD hangover, a user who mutes immediately after speaking
// would normally have their in-flight accumulator stranded — the
// hangover never fires, so the natural end-of-utterance path doesn't
// emit, and session.close's !open gate then drops anything that
// would arrive later. Flush() bypasses the hangover.
func TestAudioMonitor_FlushEmitsInFlightUtterance(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := newFakeCapture()
	mon := &audioMonitor{
		cap: fc,
		vad: audio.NewEnergyVAD(audio.VADConfig{
			Calibrate: 80 * time.Millisecond,
			// Deliberately long: we must not wait for it.
			Hangover: 30 * time.Second,
		}),
		rb:       audio.NewRingBuffer(audio.CapacityForSeconds(5)),
		log:      logger,
		flushReq: make(chan chan struct{}, 1),
	}
	mon.backend.Store("")

	var (
		emissions [][]byte
		emitMu    sync.Mutex
	)
	mon.onUtterance = func(pcm []byte) {
		emitMu.Lock()
		defer emitMu.Unlock()
		captured := make([]byte, len(pcm))
		copy(captured, pcm)
		emissions = append(emissions, captured)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	loud := loudFrame(0.6)
	silent := make([]byte, audio.FrameBytes)

	// Calibration window.
	fc.send(silent)
	fc.send(silent)
	// In-utterance: enough loud frames to clear any min-speech gate.
	for range 6 {
		fc.send(loud)
	}

	// Wait until the loop has actually consumed the frames so the
	// accumulator is populated before we flush. A polling assertion
	// on SpeechFrames is simpler than a barrier.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mon.Snapshot().SpeechFrames >= 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// No natural emission yet — hangover is 30 s.
	emitMu.Lock()
	if n := len(emissions); n != 0 {
		emitMu.Unlock()
		t.Fatalf("pre-flush: expected 0 emissions, got %d", n)
	}
	emitMu.Unlock()

	mon.Flush()

	emitMu.Lock()
	defer emitMu.Unlock()
	if len(emissions) != 1 {
		t.Fatalf("post-flush: expected exactly 1 emission, got %d", len(emissions))
	}
	if got := len(emissions[0]); got < 5*audio.FrameBytes {
		t.Errorf("flushed emission size %d < expected ≥%d", got, 5*audio.FrameBytes)
	}
}

// TestAudioMonitor_FlushWithoutUtteranceIsNoop verifies that Flush
// during silence (no accumulator content) does not produce a phantom
// emission — session.close calls Flush unconditionally, so the case
// where no speech is in flight must be a clean no-op.
func TestAudioMonitor_FlushWithoutUtteranceIsNoop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := newFakeCapture()
	mon := &audioMonitor{
		cap:      fc,
		vad:      audio.NewEnergyVAD(audio.VADConfig{Calibrate: 80 * time.Millisecond}),
		rb:       audio.NewRingBuffer(audio.CapacityForSeconds(5)),
		log:      logger,
		flushReq: make(chan chan struct{}, 1),
	}
	mon.backend.Store("")

	var called atomic.Bool
	mon.onUtterance = func(pcm []byte) { called.Store(true) }

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	silent := make([]byte, audio.FrameBytes)
	for range 4 {
		fc.send(silent)
	}
	// Let frames drain.
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		if mon.Snapshot().Frames >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mon.Flush()

	if called.Load() {
		t.Error("Flush produced an emission when accumulator was empty")
	}
}

// TestAudioMonitor_FlushBeforeStartIsNoop covers the close-before-
// start corner: Flush must not block or panic if the audio loop
// hasn't started, since session.close still runs the flush call
// unconditionally.
func TestAudioMonitor_FlushBeforeStartIsNoop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := newAudioMonitor(logger, audio.CaptureConfig{}, audio.VADConfig{})
	// Should return promptly, not block on the unmanned flushReq chan.
	done := make(chan struct{})
	go func() {
		mon.Flush()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Flush on unstarted monitor blocked")
	}
}

// fakeCapture is a hand-driven audio.Capture for tests that need to
// control the exact frame sequence the audioMonitor.loop sees. The
// frame channel is closed when the Start ctx is cancelled (matching
// SubprocessCapture's contract), so audioMonitor.Stop returns cleanly.
type fakeCapture struct {
	frames    chan audio.Frame
	closeOnce sync.Once
}

func newFakeCapture() *fakeCapture {
	return &fakeCapture{frames: make(chan audio.Frame, 64)}
}

func (f *fakeCapture) Start(ctx context.Context) (<-chan audio.Frame, error) {
	go func() {
		<-ctx.Done()
		f.closeOnce.Do(func() { close(f.frames) })
	}()
	return f.frames, nil
}

func (f *fakeCapture) Stop() error {
	f.closeOnce.Do(func() { close(f.frames) })
	return nil
}

func (f *fakeCapture) Backend() string { return "fake" }

func (f *fakeCapture) send(pcm []byte) {
	dup := make([]byte, len(pcm))
	copy(dup, pcm)
	f.frames <- audio.Frame{PCM: dup, Timestamp: time.Now()}
}

func loudFrame(amp float64) []byte {
	pcm := make([]byte, audio.FrameBytes)
	for i := range audio.FrameSamples {
		v := amp * math.Sin(2*math.Pi*440*float64(i)/audio.SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	return pcm
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
