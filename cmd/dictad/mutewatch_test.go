package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

// fakeToggler records EnsureTypeOpen / CloseIfTypeOpen calls so a test
// can assert how many transitions the watcher actually fired and in
// what order. Each call increments the corresponding counter; the
// sequence slice captures the order ("open" or "close").
type fakeToggler struct {
	opens    atomic.Uint64
	closes   atomic.Uint64
	sequence []string
}

func (f *fakeToggler) EnsureTypeOpen(_ context.Context) error {
	f.opens.Add(1)
	f.sequence = append(f.sequence, "open")
	return nil
}

func (f *fakeToggler) CloseIfTypeOpen(_ context.Context) error {
	f.closes.Add(1)
	f.sequence = append(f.sequence, "close")
	return nil
}

func discardWatcherLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func zeroFrame(n int) []byte    { return make([]byte, n) }
func nonzeroFrame(n int) []byte { f := make([]byte, n); f[0] = 1; return f }

func TestIsAllZero(t *testing.T) {
	cases := []struct {
		in   []byte
		want bool
	}{
		{[]byte{}, true},
		{[]byte{0}, true},
		{[]byte{0, 0, 0, 0}, true},
		{[]byte{0, 0, 1, 0}, false},
		{[]byte{1, 0, 0, 0}, false},
		{[]byte{0, 0, 0, 0xff}, false},
		{bytes.Repeat([]byte{0}, 2560), true},             // full FrameBytes of zeros
		{append(bytes.Repeat([]byte{0}, 2559), 1), false}, // last byte nonzero
	}
	for _, tc := range cases {
		if got := isAllZero(tc.in); got != tc.want {
			t.Errorf("isAllZero(%v...) = %v, want %v", tc.in[:min(len(tc.in), 8)], got, tc.want)
		}
	}
}

func TestMuteWatcher_NoFireBeforeFirstFrameSeedsState(t *testing.T) {
	// A single frame after construction seeds the baseline; no
	// transition should fire even though the watcher's zero-value
	// lastMuted (false) differs from a muted first frame.
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, 1)
	w.OnFrame(zeroFrame(2560))
	if tog.opens.Load() != 0 || tog.closes.Load() != 0 {
		t.Errorf("first frame must not fire; got opens=%d closes=%d", tog.opens.Load(), tog.closes.Load())
	}
}

func TestMuteWatcher_DebounceSuppressesShortGlitch(t *testing.T) {
	// Start muted. Single nonzero frame, then back to muted.
	// debounce=3 means a single-frame glitch is suppressed.
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, 3)
	w.OnFrame(zeroFrame(2560))    // seed: muted
	w.OnFrame(nonzeroFrame(2560)) // glitch
	w.OnFrame(zeroFrame(2560))    // back to muted
	w.OnFrame(zeroFrame(2560))
	if tog.opens.Load() != 0 {
		t.Errorf("debounced glitch should not have opened; got %d opens", tog.opens.Load())
	}
}

func TestMuteWatcher_FiresAfterDebounceFramesUnmute(t *testing.T) {
	// Start muted. After debounce-many consecutive nonzero frames,
	// EnsureTypeOpen fires exactly once.
	tog := &fakeToggler{}
	debounce := 3
	w := newMuteWatcher(discardWatcherLogger(), tog, debounce)
	w.OnFrame(zeroFrame(2560)) // seed: muted

	// Frames 1..debounce-1: counter increments but no fire.
	for i := 0; i < debounce-1; i++ {
		w.OnFrame(nonzeroFrame(2560))
		if tog.opens.Load() != 0 {
			t.Fatalf("fired too early (after %d non-zero frames, debounce=%d)", i+1, debounce)
		}
	}
	// Frame N: should fire.
	w.OnFrame(nonzeroFrame(2560))
	if tog.opens.Load() != 1 {
		t.Fatalf("expected 1 open after %d non-zero frames, got %d", debounce, tog.opens.Load())
	}
	// Further nonzero frames: no additional fires (no transition).
	for range 5 {
		w.OnFrame(nonzeroFrame(2560))
	}
	if tog.opens.Load() != 1 {
		t.Errorf("steady state should not refire; got %d opens", tog.opens.Load())
	}
}

func TestMuteWatcher_FullCycle(t *testing.T) {
	// Seed muted, run an unmute-mute-unmute cycle. Verify the
	// open/close sequence matches expectations.
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, 2)

	w.OnFrame(zeroFrame(2560)) // seed: muted

	// Unmute (2 frames to fire).
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	if tog.opens.Load() != 1 {
		t.Fatalf("after unmute: opens=%d want 1", tog.opens.Load())
	}

	// Mute (2 frames to fire).
	w.OnFrame(zeroFrame(2560))
	w.OnFrame(zeroFrame(2560))
	if tog.closes.Load() != 1 {
		t.Fatalf("after mute: closes=%d want 1", tog.closes.Load())
	}

	// Unmute again.
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	if tog.opens.Load() != 2 {
		t.Fatalf("after second unmute: opens=%d want 2", tog.opens.Load())
	}

	want := []string{"open", "close", "open"}
	if len(tog.sequence) != len(want) {
		t.Fatalf("sequence length: got %d want %d (%v)", len(tog.sequence), len(want), tog.sequence)
	}
	for i, s := range want {
		if tog.sequence[i] != s {
			t.Errorf("sequence[%d] = %q want %q", i, tog.sequence[i], s)
		}
	}
}

func TestMuteWatcher_CounterResetsOnReverseBeforeFire(t *testing.T) {
	// Start muted, get partway through debounce on unmute, then go
	// back to muted. The counter should reset; a subsequent full
	// debounce-worth of unmute frames should still fire correctly.
	tog := &fakeToggler{}
	debounce := 4
	w := newMuteWatcher(discardWatcherLogger(), tog, debounce)

	w.OnFrame(zeroFrame(2560)) // seed
	// 2 nonzero (< debounce), then 1 zero (resets), then 4 nonzero -> fires.
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(zeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	w.OnFrame(nonzeroFrame(2560))
	if tog.opens.Load() != 1 {
		t.Fatalf("expected exactly 1 open; got %d", tog.opens.Load())
	}
}

func TestMuteWatcher_DebounceMinClamp(t *testing.T) {
	// debounce < 1 should clamp to 1, not panic or never-fire.
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, 0)
	w.OnFrame(zeroFrame(2560))    // seed
	w.OnFrame(nonzeroFrame(2560)) // first nonzero frame fires immediately
	if tog.opens.Load() != 1 {
		t.Fatalf("debounce=0 should clamp to 1 and fire on first transition; got opens=%d", tog.opens.Load())
	}
}
