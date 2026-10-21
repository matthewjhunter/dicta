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

// fakeToggler models the session's type-mode open state the way the
// real hooks do: EnsureTypeOpen is a no-op when already open and
// CloseIfTypeOpen is a no-op when closed. opens/closes and sequence
// record only effective transitions, so a test can assert what the
// watcher actually did to the session. closeCalls counts every
// CloseIfTypeOpen call, effective or not, for idempotency checks.
type fakeToggler struct {
	opens      atomic.Uint64
	closes     atomic.Uint64
	closeCalls atomic.Uint64

	mu       sync.Mutex
	open     bool
	sequence []string
}

func (f *fakeToggler) EnsureTypeOpen(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open {
		return nil
	}
	f.open = true
	f.opens.Add(1)
	f.sequence = append(f.sequence, "open")
	return nil
}

func (f *fakeToggler) CloseIfTypeOpen(_ context.Context) error {
	f.closeCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return nil
	}
	f.open = false
	f.closes.Add(1)
	f.sequence = append(f.sequence, "close")
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

func TestMuteWatcher_SuspendClosesOpenTypeSession(t *testing.T) {
	// Regression: the watcher opened a type-mode session on unmute, then
	// `dicta suspend` landed. Suspending only stopped the watcher from
	// acting on future mute events; it left the session it had opened
	// running with no mute-driven way to close it. The daemon then sat
	// in an open type-mode session typing everything the mic heard
	// until shutdown. Suspend must close any type-mode session so the
	// mic button's "off" position still means off.
	tog, w, ch, stop := startWatcherCustom(t, testDebounce, nil)
	defer stop()

	ch <- initial(mute.Muted)
	ch <- transition(mute.Unmuted)
	settle(testDebounce)
	if o, _, _ := tog.snapshot(); o != 1 {
		t.Fatalf("precondition: unmute should have opened; got opens=%d", o)
	}

	w.Suspend("manual")

	o, c, seq := tog.snapshot()
	if o != 1 || c != 1 {
		t.Errorf("Suspend must close the type session it opened; got opens=%d closes=%d seq=%v", o, c, seq)
	}
	if len(seq) != 2 || seq[1] != "close" {
		t.Errorf("sequence = %v; want [open close]", seq)
	}

	// Idempotent: a second Suspend must not even attempt a close.
	calls := tog.closeCalls.Load()
	w.Suspend("manual")
	if got := tog.closeCalls.Load(); got != calls {
		t.Errorf("second Suspend called CloseIfTypeOpen again; calls=%d, want %d", got, calls)
	}
}

func TestMuteWatcher_FlapAutoSuspendClosesOpenTypeSession(t *testing.T) {
	// Same hazard via the flap guard: if the fire that trips the guard
	// is an open, the watcher suspends itself with the session it just
	// opened still running. Auto-suspend must close it too.
	tog, w, ch, stop := startWatcherCustom(t, testDebounce, func(w *muteWatcher) {
		w.SetFlapGuard(10*time.Second, 2)
	})
	defer stop()

	ch <- initial(mute.Muted)
	// open, close, open: the third fire exceeds threshold 2 and trips
	// the guard with the session open.
	for _, s := range []mute.State{mute.Unmuted, mute.Muted, mute.Unmuted} {
		ch <- transition(s)
		settle(testDebounce)
	}

	if susp, reason := w.Suspended(); !susp || reason != "flapping" {
		t.Fatalf("precondition: Suspended()=%v,%q; want true,\"flapping\"", susp, reason)
	}
	o, c, seq := tog.snapshot()
	if o != 2 || c != 2 {
		t.Errorf("auto-suspend must close the open type session; got opens=%d closes=%d seq=%v", o, c, seq)
	}
	if n := len(seq); n == 0 || seq[n-1] != "close" {
		t.Errorf("sequence = %v; want it to end with close", seq)
	}
}
