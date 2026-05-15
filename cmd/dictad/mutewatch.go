package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
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

	// fired is the count of transitions actually dispatched; useful
	// for tests and future status reporting.
	fired atomic.Uint64
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
		logger:   logger,
		toggler:  toggler,
		debounce: debounce,
	}
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
