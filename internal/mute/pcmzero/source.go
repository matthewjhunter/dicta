// Package pcmzero implements a mute.Source that infers mute state by
// checking captured PCM frames for all-zero bytes.
//
// Why all-zero detection: the MXL AC-44 (the mic this code was
// originally tuned against) exposes mute neither via PipeWire's
// route.mute property nor via ALSA's capture switch nor via a
// parallel USB HID interface — the touch-mute button gates the audio
// inside the device firmware and the host sees only the resulting
// PCM stream. Verified empirically on the AC-44: muted state produces
// 100% literal zeros (peak=0); unmuted-silent produces ~95% nonzero
// samples at typical ADC noise floor (-69 dBFS). The gap is so large
// that a byte-level "any nonzero?" check is enough — no threshold
// tuning required.
//
// This source only works for mics that mute by zeroing rather than
// attenuating. Mics whose mute leaves a real noise floor will read
// as "always unmuted" and emit only an Initial event. See
// mute-source-design.md §6.1.
package pcmzero

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
)

// Source is a mute.Source that classifies each PCM frame as
// muted/unmuted by checking whether every byte is zero. Frames are
// delivered via the OnFrame method, which is intended to be wired
// into an existing audio capture pump.
//
// Construction order:
//   - NewSource()        — returns an instance
//   - src.OnFrame is bound to the audio pump as its frame handler
//   - src.Watch(ctx)     — returns the event channel
//
// OnFrame is safe to call before Watch; observations made earlier
// than Watch are dropped (the source has no consumer yet), which is
// fine because Watch's Initial event is seeded from the first frame
// observed AFTER Watch returns. In practice the audio pump is started
// after Watch by the daemon's main flow.
type Source struct {
	logger *slog.Logger

	mu      sync.Mutex
	out     chan mute.Event // nil until Watch is called
	lastSet bool
	last    mute.State
}

// NewSource constructs a pcm-zero source. logger may be nil; a
// discard logger is substituted.
func NewSource(logger *slog.Logger) *Source {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Source{logger: logger}
}

func (s *Source) Name() string { return "pcm-zero" }
func (s *Source) Describe() string {
	return "PCM all-zero detection on the captured audio frame stream"
}

// Watch begins emitting events. Must be called exactly once per
// Source instance.
func (s *Source) Watch(ctx context.Context) (<-chan mute.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil {
		// Second Watch call. The interface contract says Watch is
		// not re-entrant; surface the misuse rather than papering
		// over it.
		return nil, errPcmzeroWatchTwice
	}
	s.out = make(chan mute.Event, 1)
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		ch := s.out
		s.out = nil
		s.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()
	return s.out, nil
}

// OnFrame is the audio-pump frame hook. It is cheap (a byte scan
// over a typically-2560-byte frame) and non-blocking — events are
// only emitted on state transitions.
func (s *Source) OnFrame(pcm []byte) {
	st := mute.Unmuted
	if isAllZero(pcm) {
		st = mute.Muted
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		// Frame observed before Watch was called, or after ctx was
		// cancelled. Drop silently.
		return
	}

	initial := false
	if !s.lastSet {
		s.lastSet = true
		s.last = st
		initial = true
	} else if st == s.last {
		// Filter consecutive identical observations so the watcher's
		// debounce counter only ticks on real changes.
		return
	} else {
		s.last = st
	}

	ev := mute.Event{
		State:   st,
		At:      time.Now(),
		Source:  s.Name(),
		Initial: initial,
	}

	// Drop-newest-on-full: if the watcher has fallen behind, prefer
	// the most recent state over stale state. State is idempotent so
	// losing an intermediate observation is fine as long as the
	// final state arrives.
	select {
	case s.out <- ev:
	default:
		// Channel full; try to displace the queued event with this
		// newer one.
		select {
		case <-s.out:
		default:
		}
		select {
		case s.out <- ev:
		default:
		}
	}
}

// isAllZero reports whether every byte of pcm is zero. A nil/empty
// slice is treated as "all zero"; in practice the audio pump never
// delivers such a frame.
func isAllZero(pcm []byte) bool {
	for _, b := range pcm {
		if b != 0 {
			return false
		}
	}
	return true
}

// errPcmzeroWatchTwice surfaces the (unexpected) double-Watch case.
var errPcmzeroWatchTwice = pcmzeroError("pcmzero.Source.Watch called more than once")

type pcmzeroError string

func (e pcmzeroError) Error() string { return string(e) }
