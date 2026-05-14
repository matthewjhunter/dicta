package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// muteWatcher implements option C of the unmute-to-dictate design: it
// consumes the audio capture frame stream, decides whether the mic is
// hardware-muted by checking for all-zero PCM, and toggles a type-mode
// session on transitions.
//
// Why all-zero detection: the MXL AC-44 (the one mic this code has
// been tuned against) exposes mute neither via PipeWire's route.mute
// property nor via ALSA's capture switch nor via a parallel USB HID
// interface — the button gates the audio inside the device firmware
// and the host sees only the resulting PCM stream. Verified
// empirically on the AC-44: muted state produces 100% literal zeros
// (peak=0); unmuted-silent produces ~95% nonzero samples at typical
// ADC noise floor (-69 dBFS). The gap is so large that a byte-level
// "any nonzero?" check is enough — no threshold tuning required.
//
// This is a single-mic feature. Other mics that mute by silencing
// rather than zeroing will read as "always unmuted" and the watcher
// will never fire. CONFIGURATION.md has a probe recipe operators can
// run to check their hardware before enabling --unmute-to-dictate.
//
// Debounce: a transition must persist for at least debounceFrames
// consecutive frames before firing, so a single glitch doesn't
// open/close a session. With 80 ms frames, debounceFrames=13 ≈ 1 s of
// effective latency.
//
// Clip-mode safety: when the user has explicitly opened clip-mode
// (Scroll Lock), we do NOT close it on a mute event and we do NOT
// reopen type-mode on an unmute event — explicit user gestures win
// over inferred ones.
type muteWatcher struct {
	logger         *slog.Logger
	toggler        muteToggler
	debounceFrames int

	// counter, lastMuted, and started are accessed only from the audio
	// pump goroutine (audioMonitor.loop), so no mutex is needed.
	counter   int
	lastMuted bool
	started   bool

	// fired is the count of transitions actually dispatched, exposed
	// for tests and future status reporting.
	fired atomic.Uint64
}

// muteToggler is the narrow slice of session that the watcher needs.
// Defined here (not in session.go) so the watcher is decoupled from
// the full session surface and easy to fake in tests.
type muteToggler interface {
	// EnsureTypeOpen opens type-mode if no session is currently open.
	// A session that is already open in either mode is left alone —
	// in particular we never disturb an explicit clip-mode session.
	EnsureTypeOpen(ctx context.Context) error

	// CloseIfTypeOpen closes the session if and only if it is open in
	// type-mode. Clip-mode sessions and already-closed sessions are
	// left alone.
	CloseIfTypeOpen(ctx context.Context) error
}

func newMuteWatcher(logger *slog.Logger, toggler muteToggler, debounceFrames int) *muteWatcher {
	if debounceFrames < 1 {
		debounceFrames = 1
	}
	return &muteWatcher{
		logger:         logger,
		toggler:        toggler,
		debounceFrames: debounceFrames,
	}
}

// OnFrame is the audioMonitor frame hook. It must be cheap; the audio
// pump back-pressures if this handler blocks. The toggler calls inside
// the transition path use a bounded context so a wedged Toggle doesn't
// stall the audio loop.
func (w *muteWatcher) OnFrame(pcm []byte) {
	muted := isAllZero(pcm)

	if !w.started {
		// First frame seeds lastMuted so we never fire a transition at
		// startup just because the mic happens to be in whichever
		// state. The user has to do a real mute/unmute action to
		// invoke the watcher.
		w.lastMuted = muted
		w.started = true
		return
	}

	if muted == w.lastMuted {
		w.counter = 0
		return
	}

	w.counter++
	if w.counter < w.debounceFrames {
		return
	}

	w.counter = 0
	w.lastMuted = muted
	w.fired.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if muted {
		w.logger.Info("mutewatch: mic muted, closing type-mode if open")
		if err := w.toggler.CloseIfTypeOpen(ctx); err != nil {
			w.logger.Warn("mutewatch: CloseIfTypeOpen failed", "err", err)
		}
	} else {
		w.logger.Info("mutewatch: mic unmuted, opening type-mode")
		if err := w.toggler.EnsureTypeOpen(ctx); err != nil {
			w.logger.Warn("mutewatch: EnsureTypeOpen failed", "err", err)
		}
	}
}

// isAllZero reports whether every byte of pcm is zero. A nil/empty
// slice is treated as "all zero" but the audioMonitor never delivers
// such a frame — it only emits FrameBytes-length chunks.
func isAllZero(pcm []byte) bool {
	for _, b := range pcm {
		if b != 0 {
			return false
		}
	}
	return true
}
