package mute

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Auto is the source that ships when the user enables
// --unmute-to-dictate without pinning an explicit source. It starts
// every wrapped source concurrently and forwards their events. The
// first non-Initial transition that arrives "locks in" the source
// that produced it; subsequent events from other sources are dropped
// and the unused sources are cancelled. This implements the §3.1
// auto-detect-on-enable model from mute-source-design.md.
//
// Auto is safe to use only because the enclosing feature
// (--unmute-to-dictate) is itself default-off (§3). It should not be
// generalized to other surfaces without re-examining the threat
// model.
type Auto struct {
	logger *slog.Logger
	subs   []Source
}

// NewAuto constructs an Auto source over the given subsources. The
// passed slice is referenced as-is; callers should not mutate it
// after construction. At least one subsource is required.
func NewAuto(logger *slog.Logger, subs []Source) (*Auto, error) {
	if len(subs) == 0 {
		return nil, fmt.Errorf("mute.NewAuto: at least one subsource required")
	}
	return &Auto{logger: logger, subs: subs}, nil
}

func (a *Auto) Name() string { return "auto" }

func (a *Auto) Describe() string {
	names := make([]string, len(a.subs))
	for i, s := range a.subs {
		names[i] = s.Name()
	}
	return fmt.Sprintf("auto over %d sources: %v", len(a.subs), names)
}

// Subsources returns the wrapped sources for diagnostic surfaces
// (probe-mute). The returned slice is the same one the Auto holds —
// do not mutate.
func (a *Auto) Subsources() []Source { return a.subs }

// Watch starts every subsource, forwards Initial events from each,
// then locks to whichever subsource produces the first real
// transition. Subsources that fail to Start are logged at WARN and
// excluded; if every subsource fails, Watch returns an error.
//
// Implementation note: each subsource gets its own child context so
// the lock-in logic can cancel the LOSERS without also killing the
// WINNER. An earlier implementation cancelled a single shared
// subcontext on first transition, which silently killed the locked
// source's event stream — first transition fired, nothing fired
// after. The per-source cancel pattern below avoids that.
func (a *Auto) Watch(ctx context.Context) (<-chan Event, error) {
	type running struct {
		src    Source
		ch     <-chan Event
		cancel context.CancelFunc
	}
	var live []running
	for _, s := range a.subs {
		subCtx, cancel := context.WithCancel(ctx)
		ch, err := s.Watch(subCtx)
		if err != nil {
			cancel()
			a.logger.Warn("mute.auto: subsource failed to start",
				"source", s.Name(), "err", err)
			continue
		}
		live = append(live, running{src: s, ch: ch, cancel: cancel})
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("mute.auto: no subsources started")
	}

	out := make(chan Event, 8)

	var (
		mu     sync.Mutex
		locked string // empty until a real transition arrives
	)

	// cancelLosersLocked stops every subsource except `winner`. Caller
	// holds mu. Idempotent: subsequent calls after a winner is set are
	// no-ops because losers are already cancelled.
	cancelLosersLocked := func(winner string) {
		for _, r := range live {
			if r.src.Name() != winner {
				r.cancel()
			}
		}
		a.logger.Info("mute.auto: locked to source",
			"source", winner, "candidates", len(live))
	}

	var wg sync.WaitGroup
	for _, r := range live {
		wg.Add(1)
		go func(r running) {
			defer wg.Done()
			for ev := range r.ch {
				mu.Lock()
				if ev.Initial {
					// Always forward initial events; they don't fire
					// the watcher (Initial=true is its own signal).
					mu.Unlock()
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
					continue
				}
				// Real transition. If nobody has locked yet, this
				// source wins and the losers get cancelled. If
				// somebody else has locked, drop the event quietly —
				// our own cancel will already have fired and our
				// channel will close shortly.
				if locked == "" {
					locked = r.src.Name()
					cancelLosersLocked(locked)
				}
				winning := locked == r.src.Name()
				mu.Unlock()
				if !winning {
					a.logger.Info("mute.auto: dropping event from non-locked source",
						"source", r.src.Name(), "state", ev.State.String())
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}(r)
	}

	go func() {
		wg.Wait()
		// Make sure any straggler goroutines see cancellation when
		// the parent ctx closes.
		for _, r := range live {
			r.cancel()
		}
		close(out)
	}()

	return out, nil
}
