package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/audio"
)

// framesPCM concatenates frames into one accumulator-shaped buffer.
func framesPCM(frames ...[]byte) []byte {
	var b bytes.Buffer
	for _, f := range frames {
		b.Write(f)
	}
	return b.Bytes()
}

func TestQuietestSplit(t *testing.T) {
	loud := loudFrame(0.6)
	quiet := loudFrame(0.02)
	fb := audio.FrameBytes

	tests := []struct {
		name   string
		frames [][]byte
		search int
		want   int
	}{
		{
			name: "cuts after the pause inside the window",
			// 10 frames; window is capped at half = last 5 (indices 5-9).
			frames: [][]byte{loud, loud, loud, loud, loud, loud, loud, quiet, loud, loud},
			search: splitSearchBytes,
			want:   8 * fb,
		},
		{
			name: "ignores a pause before the window",
			// The pause at index 2 would leave a tiny first chunk.
			frames: [][]byte{loud, loud, quiet, loud, loud, loud, loud, loud, loud, loud},
			search: splitSearchBytes,
			want:   10 * fb,
		},
		{
			name:   "no pause cuts at the end, like a plain cap",
			frames: [][]byte{loud, loud, loud, loud, loud, loud},
			search: splitSearchBytes,
			want:   6 * fb,
		},
		{
			name: "search window bounds the look-back",
			// Window of 2 frames (indices 8-9) excludes the pause at 7.
			frames: [][]byte{loud, loud, loud, loud, loud, loud, loud, quiet, loud, loud},
			search: 2 * fb,
			want:   10 * fb,
		},
		{
			name:   "single frame",
			frames: [][]byte{loud},
			search: splitSearchBytes,
			want:   fb,
		},
		{
			name:   "empty",
			search: splitSearchBytes,
			want:   0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quietestSplit(framesPCM(tc.frames...), tc.search); got != tc.want {
				t.Errorf("quietestSplit = %d frames, want %d frames", got/fb, tc.want/fb)
			}
		})
	}
}

// TestAudioMonitor_MaxUtteranceCap_SplitsAtPause drives the real loop: when
// the cap is reached, the chunk must end at the pause, and the audio after
// the pause must lead the next chunk rather than being lost or duplicated.
func TestAudioMonitor_MaxUtteranceCap_SplitsAtPause(t *testing.T) {
	fc := newFakeCapture()
	mon := &audioMonitor{
		cap: fc,
		vad: audio.NewEnergyVAD(audio.VADConfig{
			Calibrate: 80 * time.Millisecond,
			// Long hangover: only the cap may end a chunk here.
			Hangover: 30 * time.Second,
		}),
		rb:       audio.NewRingBuffer(audio.CapacityForSeconds(5)),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		flushReq: make(chan chan struct{}, 1),
	}
	mon.backend.Store("")
	mon.SetMaxUtterance(12 * audio.FrameBytes)

	var (
		emissions [][]byte
		emitMu    sync.Mutex
	)
	mon.onUtterance = func(pcm []byte) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emissions = append(emissions, append([]byte(nil), pcm...))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := mon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = mon.Stop() }()

	silent := make([]byte, audio.FrameBytes)
	fc.send(silent) // calibration
	fc.send(silent)

	// 12 speech frames reach the cap. Distinct amplitudes after the pause
	// let the test tell carried frames apart from the rest.
	loud := loudFrame(0.6)
	quiet := loudFrame(0.02)
	tailA, tailB := loudFrame(0.5), loudFrame(0.4)
	for range 9 {
		fc.send(loud)
	}
	fc.send(quiet)
	fc.send(tailA)
	fc.send(tailB)

	// Two more frames, then flush the open chunk.
	after := loudFrame(0.3)
	fc.send(after)
	fc.send(after)

	waitFor := func(n int) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			emitMu.Lock()
			got := len(emissions)
			emitMu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(1)
	// Let the two trailing frames reach the accumulator before flushing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && mon.frames.Load() < 16 {
		time.Sleep(10 * time.Millisecond)
	}
	mon.Flush()
	waitFor(2)

	emitMu.Lock()
	defer emitMu.Unlock()
	if len(emissions) != 2 {
		t.Fatalf("emissions: got %d want 2", len(emissions))
	}
	fb := audio.FrameBytes
	if got := len(emissions[0]) / fb; got != 10 {
		t.Errorf("first chunk: got %d frames, want 10 (ending on the pause)", got)
	}
	if !bytes.Equal(emissions[0][9*fb:], quiet) {
		t.Error("first chunk should end with the quiet frame")
	}
	want := framesPCM(tailA, tailB, after, after)
	if !bytes.Equal(emissions[1], want) {
		t.Errorf("second chunk: got %d frames, want the 2 carried frames + 2 new ones (%d frames)",
			len(emissions[1])/fb, len(want)/fb)
	}
}
