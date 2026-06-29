package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
)

// fakeToggler records EnsureTypeOpen / CloseIfTypeOpen calls so a
// test can assert how many transitions the watcher actually fired
// and in what order.
type fakeToggler struct {
	opens  atomic.Uint64
	closes atomic.Uint64

	mu       sync.Mutex
	sequence []string
}

func (f *fakeToggler) EnsureTypeOpen(_ context.Context) error {
	f.opens.Add(1)
	f.mu.Lock()
	f.sequence = append(f.sequence, "open")
	f.mu.Unlock()
	return nil
}

func (f *fakeToggler) CloseIfTypeOpen(_ context.Context) error {
	f.closes.Add(1)
	f.mu.Lock()
	f.sequence = append(f.sequence, "close")
	f.mu.Unlock()
	return nil
}

func (f *fakeToggler) snapshot() (uint64, uint64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := make([]string, len(f.sequence))
	copy(seq, f.sequence)
	return f.opens.Load(), f.closes.Load(), seq
}

func discardWatcherLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testDebounce keeps unit tests fast. settleSlack is added after a
// transition is pushed so the timer has a comfortable window to fire.
const (
	testDebounce = 20 * time.Millisecond
	settleSlack  = 80 * time.Millisecond
)

// startWatcher boots a muteWatcher under test and returns hooks for
// sending events and shutting down. The watcher runs on a goroutine
// until stop() is called.
func startWatcher(t *testing.T, debounce time.Duration) (*fakeToggler, chan<- mute.Event, func()) {
	t.Helper()
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, debounce)
	ch := make(chan mute.Event, 32)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, ch)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher.Run did not return within 2s")
		}
	}
	return tog, ch, stop
}

func settle(debounce time.Duration) {
	time.Sleep(debounce + settleSlack)
}

func initial(s mute.State) mute.Event {
	return mute.Event{State: s, Source: "test", Initial: true}
}

func transition(s mute.State) mute.Event {
	return mute.Event{State: s, Source: "test"}
}

func TestMuteWatcher_NoFireOnInitialEvent(t *testing.T) {
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()
	ch <- initial(mute.Muted)
	settle(testDebounce)

	o, c, _ := tog.snapshot()
	if o != 0 || c != 0 {
		t.Errorf("Initial event should not fire; got opens=%d closes=%d", o, c)
	}
}

func TestMuteWatcher_DebounceSuppressesShortGlitch(t *testing.T) {
	// Seed muted, transient unmute glitch followed immediately by
	// remute. Reversal must arrive within debounce window and cancel
	// the pending fire.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	// Reversal sent immediately (within debounce):
	ch <- transition(mute.Muted)
	settle(testDebounce)

	o, c, _ := tog.snapshot()
	if o != 0 || c != 0 {
		t.Errorf("debounced glitch should not fire; got opens=%d closes=%d", o, c)
	}
}

func TestMuteWatcher_FiresAfterDebounceHoldsUnmute(t *testing.T) {
	// Seed muted, then a single unmute transition. Wait the debounce
	// window and observe one EnsureTypeOpen fire.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	o, _, _ := tog.snapshot()
	if o != 1 {
		t.Errorf("expected 1 open after stable unmute; got opens=%d", o)
	}
}

func TestMuteWatcher_NoRefireOnRedundantEvents(t *testing.T) {
	// After a transition fires, redundant events (same state as the
	// new lastState) must not refire.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	settle(testDebounce)
	// Send extra redundant Unmuted "transitions" (defensive: a
	// misbehaving source might re-emit; the watcher must not fire).
	for range 5 {
		ch <- transition(mute.Unmuted)
	}
	settle(testDebounce)

	o, _, _ := tog.snapshot()
	if o != 1 {
		t.Errorf("redundant events should not refire; got opens=%d", o)
	}
}

func TestMuteWatcher_FullCycle(t *testing.T) {
	// Seed muted; cycle unmute, mute, unmute. Each transition is
	// followed by a settle so the timer can fire before the next.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	settle(testDebounce)
	ch <- transition(mute.Muted)
	settle(testDebounce)
	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	o, c, seq := tog.snapshot()
	if o != 2 || c != 1 {
		t.Errorf("opens=%d closes=%d; want opens=2 closes=1", o, c)
	}
	want := []string{"open", "close", "open"}
	if len(seq) != len(want) {
		t.Fatalf("sequence length: got %d want %d (%v)", len(seq), len(want), seq)
	}
	for i, s := range want {
		if seq[i] != s {
			t.Errorf("sequence[%d] = %q want %q", i, seq[i], s)
		}
	}
}

func TestMuteWatcher_ReverseBeforeFireThenForward(t *testing.T) {
	// Pending unmute gets reversed back to muted, then a fresh unmute
	// arrives and is allowed to settle. Exactly one open fires.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted) // start pending
	ch <- transition(mute.Muted)   // reverse before window closes
	// Wait less than debounce so we don't accidentally fire anything.
	time.Sleep(testDebounce / 4)
	ch <- transition(mute.Unmuted) // start a fresh pending
	settle(testDebounce)

	o, c, _ := tog.snapshot()
	if o != 1 || c != 0 {
		t.Errorf("opens=%d closes=%d; want opens=1 closes=0", o, c)
	}
}

func TestMuteWatcher_MinimalDebounceStillFires(t *testing.T) {
	// 0 debounce should clamp to a minimum (1ms) inside the watcher,
	// not panic or never fire.
	tog, ch, stop := startWatcher(t, 0)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	settle(50 * time.Millisecond)

	o, _, _ := tog.snapshot()
	if o != 1 {
		t.Errorf("zero-debounce should clamp to a small value and fire; got opens=%d", o)
	}
}

// startWatcherCustom is like startWatcher but lets the caller tweak the
// watcher (flap guard, notify hook, device) before Run starts.
func startWatcherCustom(t *testing.T, debounce time.Duration, configure func(*muteWatcher)) (*fakeToggler, *muteWatcher, chan<- mute.Event, func()) {
	t.Helper()
	tog := &fakeToggler{}
	w := newMuteWatcher(discardWatcherLogger(), tog, debounce)
	if configure != nil {
		configure(w)
	}
	ch := make(chan mute.Event, 64)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, ch)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher.Run did not return within 2s")
		}
	}
	return tog, w, ch, stop
}

func TestMuteWatcher_SuspendIgnoresTransitions(t *testing.T) {
	tog, w, ch, stop := startWatcherCustom(t, testDebounce, nil)
	defer stop()

	ch <- initial(mute.Muted)
	w.Suspend("manual")
	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	o, c, _ := tog.snapshot()
	if o != 0 || c != 0 {
		t.Errorf("suspended watcher must not fire; got opens=%d closes=%d", o, c)
	}
	if susp, reason := w.Suspended(); !susp || reason != "manual" {
		t.Errorf("Suspended()=%v,%q; want true,\"manual\"", susp, reason)
	}
}

func TestMuteWatcher_ResumeReenablesFiring(t *testing.T) {
	tog, w, ch, stop := startWatcherCustom(t, testDebounce, nil)
	defer stop()

	ch <- initial(mute.Muted)
	w.Suspend("manual")
	w.Resume()
	if susp, _ := w.Suspended(); susp {
		t.Fatalf("watcher still suspended after Resume")
	}
	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	o, _, _ := tog.snapshot()
	if o != 1 {
		t.Errorf("resumed watcher should fire; got opens=%d", o)
	}
}

func TestMuteWatcher_NoStaleFireAfterResume(t *testing.T) {
	// While suspended the watcher tracks state from events. A transition
	// observed during suspension must not be replayed on Resume.
	tog, w, ch, stop := startWatcherCustom(t, testDebounce, nil)
	defer stop()

	ch <- initial(mute.Muted)
	w.Suspend("manual")
	ch <- transition(mute.Unmuted) // observed-but-ignored; lastState -> unmuted
	settle(testDebounce)
	w.Resume()
	settle(testDebounce) // no new events after resume

	o, c, _ := tog.snapshot()
	if o != 0 || c != 0 {
		t.Errorf("Resume must not replay a transition seen while suspended; got opens=%d closes=%d", o, c)
	}
}

func TestMuteWatcher_FlapAutoSuspends(t *testing.T) {
	var (
		nmu     sync.Mutex
		notifns int
	)
	_, w, ch, stop := startWatcherCustom(t, testDebounce, func(w *muteWatcher) {
		w.SetFlapGuard(10*time.Second, 3)
		w.SetDevice("alsa_input.test")
		w.SetNotify(func(_, _ string) {
			nmu.Lock()
			notifns++
			nmu.Unlock()
		})
	})
	defer stop()

	ch <- initial(mute.Muted)
	// Four fires (open, close, open, close) within the window; threshold
	// is 3, so the fourth trips the guard.
	states := []mute.State{mute.Unmuted, mute.Muted, mute.Unmuted, mute.Muted}
	for _, s := range states {
		ch <- transition(s)
		settle(testDebounce)
	}
	// A further transition must be ignored now that we are suspended.
	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	if susp, reason := w.Suspended(); !susp || reason != "flapping" {
		t.Errorf("Suspended()=%v,%q; want true,\"flapping\"", susp, reason)
	}
	if got := w.fired.Load(); got != 4 {
		t.Errorf("fired=%d; want 4 (guard trips on the 4th, later transitions ignored)", got)
	}
	nmu.Lock()
	n := notifns
	nmu.Unlock()
	if n != 1 {
		t.Errorf("notify called %d times; want exactly 1 on auto-suspend", n)
	}
}

func TestMuteWatcher_FlapGuardDisabled(t *testing.T) {
	// threshold <= 0 disables the guard: rapid flapping keeps firing.
	_, w, ch, stop := startWatcherCustom(t, testDebounce, func(w *muteWatcher) {
		w.SetFlapGuard(10*time.Second, 0)
	})
	defer stop()

	ch <- initial(mute.Muted)
	for _, s := range []mute.State{mute.Unmuted, mute.Muted, mute.Unmuted, mute.Muted, mute.Unmuted} {
		ch <- transition(s)
		settle(testDebounce)
	}

	if susp, _ := w.Suspended(); susp {
		t.Errorf("guard disabled (threshold 0) must never auto-suspend")
	}
	if got := w.fired.Load(); got != 5 {
		t.Errorf("fired=%d; want 5 with guard disabled", got)
	}
}

func TestMuteWatcher_HandlesUnseededTransitionDefensively(t *testing.T) {
	// A source that skips its Initial event sends a transition first.
	// The watcher must treat it as a seed and not fire.
	tog, ch, stop := startWatcher(t, testDebounce)
	defer stop()

	ch <- transition(mute.Unmuted)
	settle(testDebounce)

	o, c, _ := tog.snapshot()
	if o != 0 || c != 0 {
		t.Errorf("unseeded transition should be treated as seed; got opens=%d closes=%d", o, c)
	}
}
