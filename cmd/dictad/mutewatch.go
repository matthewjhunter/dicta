package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
)

const (
	// defaultFlapWindow and defaultFlapThreshold configure the
	// auto-suspend guard. If more than defaultFlapThreshold transitions
	// fire within defaultFlapWindow, the watcher suspends itself: this
	// is the signature of a noise-gated, non-dictation device (e.g. a
	// gaming headset switched in as the system default) whose gate
	// crosses zero repeatedly, which would otherwise loop the
	// session-open/close mic-cue beep indefinitely. Both are
	// overridable via SetFlapGuard (wired to --unmute-flap-window and
	// --unmute-flap-threshold).
	defaultFlapWindow    = 10 * time.Second
	defaultFlapThreshold = 6
)

// muteWatcher consumes mute.Event observations from a mute.Source and
// fires session transitions when a new state holds steady for at least
// debounceDuration before being reversed.
//
// The watcher is independent of how the source determines mute state
// — pcm-zero, pipewire, evdev, or the auto multiplexer all reach it
// the same way.
//
// Debounce model: edge-triggered. Sources emit one event per real
// observed state change (they filter consecutive identical
// observations themselves). When a transition arrives, we arm a
// timer for debounceDuration; if the timer fires without an
// opposite-direction event arriving first, the transition is
// "real" and we call the toggler. A reversing event before the
// timer fires cancels it — the user pressed-and-released too
// quickly to count as a real intent.
//
// Clip-mode safety: the muteToggler interface only opens/closes
// type-mode. Clip-mode sessions are not disturbed by mute events;
// the toggler implementation enforces that.
type muteWatcher struct {
	logger   *slog.Logger
	toggler  muteToggler
	debounce time.Duration

	// flapWindow / flapThreshold configure the auto-suspend guard. A
	// threshold <= 0 disables the guard entirely. Set once before Run
	// via SetFlapGuard; not mutated afterwards.
	flapWindow    time.Duration
	flapThreshold int

	// notify, if set, is called once when the watcher auto-suspends on
	// flap detection, to surface the event on the desktop. nil = no
	// desktop notification (the WARN log still fires).
	notify func(title, body string)

	// device is the human-facing mic identifier used in the
	// auto-suspend notification body. Empty renders as "default mic".
	device string

	// now is the clock, injectable for deterministic flap tests.
	// Defaults to time.Now.
	now func() time.Time

	// fired is the count of transitions actually dispatched; useful
	// for tests and future status reporting.
	fired atomic.Uint64

	// mu guards the suspend state and the flap window, which are
	// touched by Run (fire path), Suspend, and Resume from different
	// goroutines.
	mu            sync.Mutex
	suspended     bool
	suspendReason string
	fireTimes     []time.Time
}

// muteToggler is the narrow slice of session that the watcher needs.
type muteToggler interface {
	EnsureTypeOpen(ctx context.Context) error
	CloseIfTypeOpen(ctx context.Context) error
}

func newMuteWatcher(logger *slog.Logger, toggler muteToggler, debounce time.Duration) *muteWatcher {
	if debounce <= 0 {
		debounce = time.Millisecond // smallest meaningful default
	}
	return &muteWatcher{
		logger:        logger,
		toggler:       toggler,
		debounce:      debounce,
		flapWindow:    defaultFlapWindow,
		flapThreshold: defaultFlapThreshold,
		now:           time.Now,
	}
}

// SetFlapGuard overrides the auto-suspend guard parameters. A
// threshold <= 0 disables the guard. Call before Run.
func (w *muteWatcher) SetFlapGuard(window time.Duration, threshold int) {
	w.flapWindow = window
	w.flapThreshold = threshold
}

// SetNotify installs the desktop-notification callback fired once on
// auto-suspend. Call before Run.
func (w *muteWatcher) SetNotify(fn func(title, body string)) { w.notify = fn }

// SetDevice records the mic identifier shown in the auto-suspend
// notification. Call before Run.
func (w *muteWatcher) SetDevice(name string) { w.device = name }

// Suspend stops the watcher from acting on mute transitions until
// Resume is called. reason is recorded for status/logging ("manual",
// "flapping"). Safe to call concurrently with Run. Idempotent.
func (w *muteWatcher) Suspend(reason string) {
	w.mu.Lock()
	already := w.suspended
	w.suspended = true
	w.suspendReason = reason
	w.mu.Unlock()
	if !already {
		w.logger.Info("mutewatch: suspended", "reason", reason)
	}
}

// Resume re-enables acting on mute transitions and clears the flap
// window. The watcher tracks mute state while suspended, so resuming
// does not replay a transition observed during suspension. Safe to
// call concurrently with Run. Idempotent.
func (w *muteWatcher) Resume() {
	w.mu.Lock()
	was := w.suspended
	w.suspended = false
	w.suspendReason = ""
	w.fireTimes = nil
	w.mu.Unlock()
	if was {
		w.logger.Info("mutewatch: resumed")
	}
}

// Suspended reports whether the watcher is currently suspended and,
// if so, why. Safe to call concurrently with Run.
func (w *muteWatcher) Suspended() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.suspended, w.suspendReason
}

// recordFireAndCheckFlap appends now to the flap window, prunes stale
// entries, and returns true if this fire pushed the count past the
// threshold — in which case it also flips the watcher to suspended
// with reason "flapping". Returns false when the guard is disabled or
// already suspended.
func (w *muteWatcher) recordFireAndCheckFlap(now time.Time) bool {
	if w.flapThreshold <= 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.suspended {
		return false
	}
	cutoff := now.Add(-w.flapWindow)
	kept := w.fireTimes[:0]
	for _, t := range w.fireTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.fireTimes = append(kept, now)
	if len(w.fireTimes) > w.flapThreshold {
		w.suspended = true
		w.suspendReason = "flapping"
		w.fireTimes = nil
		return true
	}
	return false
}

// Run consumes events from ch and fires transitions. It returns when
// ch is closed or ctx is cancelled. Initial events seed lastState
// without firing; subsequent transitions must hold for the debounce
// duration without being reversed to fire.
func (w *muteWatcher) Run(ctx context.Context, ch <-chan mute.Event) {
	var (
		lastState  mute.State
		seeded     bool
		pending    mute.State
		hasPending bool
		timer      *time.Timer
		timerC     <-chan time.Time
	)

	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
		hasPending = false
	}
	armTimer := func(state mute.State) {
		if timer != nil {
			timer.Stop()
		}
		pending = state
		hasPending = true
		timer = time.NewTimer(w.debounce)
		timerC = timer.C
	}
	fire := func(state mute.State) {
		// Gate on suspend, re-checked here because a manual Suspend may
		// land between arming the debounce timer and its expiry. Track
		// lastState regardless so a later Resume sees the current state
		// and does not synthesize a stale transition.
		if susp, _ := w.Suspended(); susp {
			lastState = state
			return
		}
		lastState = state
		w.fired.Add(1)
		tCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		switch state {
		case mute.Muted:
			w.logger.Info("mutewatch: mic muted, closing type-mode if open")
			if err := w.toggler.CloseIfTypeOpen(tCtx); err != nil {
				w.logger.Warn("mutewatch: CloseIfTypeOpen failed", "err", err)
			}
		case mute.Unmuted:
			w.logger.Info("mutewatch: mic unmuted, opening type-mode")
			if err := w.toggler.EnsureTypeOpen(tCtx); err != nil {
				w.logger.Warn("mutewatch: EnsureTypeOpen failed", "err", err)
			}
		}
		if w.recordFireAndCheckFlap(w.now()) {
			w.logger.Warn("mutewatch: auto-suspended after rapid mute flapping",
				"device", w.device, "window", w.flapWindow, "threshold", w.flapThreshold)
			if w.notify != nil {
				dev := w.device
				if dev == "" {
					dev = "default mic"
				}
				w.notify("dicta: dictation suspended",
					fmt.Sprintf("Mute detection is flapping on %s. Auto-activation suspended -- run `dicta resume` to re-enable.", dev))
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			stopTimer()
			return
		case <-timerC:
			// Debounce elapsed: the pending state never got reversed,
			// so this is a real transition.
			fired := pending
			stopTimer()
			fire(fired)
		case ev, ok := <-ch:
			if !ok {
				stopTimer()
				return
			}
			if ev.Initial {
				lastState = ev.State
				seeded = true
				stopTimer()
				w.logger.Info("mutewatch: seeded state",
					"source", ev.Source, "state", ev.State.String())
				continue
			}
			if !seeded {
				// Defensive: a source emitted a transition before its
				// Initial event. Treat as seed.
				lastState = ev.State
				seeded = true
				continue
			}
			if susp, _ := w.Suspended(); susp {
				// Suspended (manually or by flap auto-suspend): track
				// the current state so Resume does not synthesize a
				// stale transition, but do not arm or fire.
				lastState = ev.State
				stopTimer()
				continue
			}
			if ev.State == lastState {
				// Reversal before the debounce window expired —
				// cancel the pending fire. (Or a redundant event
				// from a source that didn't filter; treat the same.)
				stopTimer()
				continue
			}
			// Real candidate transition. Arm/re-arm the debounce.
			if hasPending && pending == ev.State {
				// Same pending direction; timer already running, do
				// nothing (don't reset the clock just because the
				// source re-asserted).
				continue
			}
			armTimer(ev.State)
		}
	}
}
