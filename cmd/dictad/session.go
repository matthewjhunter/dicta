package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/dispatch"
)

// sessionMode names the dictation mode. Per D6 the type and clip modes
// are mutually exclusive: opening one while the other is open closes
// the active mode first.
type sessionMode int

const (
	modeNone sessionMode = iota
	modeType
	modeClip // reserved for phase 9 (dicta-preview panel)
)

func (m sessionMode) String() string {
	switch m {
	case modeNone:
		return "none"
	case modeType:
		return "type"
	case modeClip:
		return "clip"
	default:
		return "unknown"
	}
}

// session is the type-mode (and eventually clip-mode) state machine.
// Phase 7 lights up modeType only; modeClip plumbing exists but
// Toggle/Open for it return ErrClipNotImplemented until phase 9.
//
// The state machine guarantees that:
//   - At most one mode is open at a time (D6).
//   - VAD is reset on every open so calibration restarts against
//     fresh silence (§5.1: "first 500ms of each opened session").
//   - In-flight transcripts that arrive after the session that
//     produced them has been closed do NOT type — the user toggled off
//     in the gap between speech and transcript and they expect
//     keystrokes to stop.
//
// The closure-vs-epoch gate: each utterance handler captures the epoch
// at submit time. When the session closes (or reopens) we bump the
// epoch. Stale handlers compare epochs and drop instead of typing.
type session struct {
	logger *slog.Logger
	typer  dispatch.Typer
	cuer   audio.Cuer
	asrMon *asrMonitor
	vad    audio.VAD // optional: Reset() called on session-open if non-nil

	// daemonCtx is the long-lived parent ctx for typer dispatch. We do
	// NOT derive a per-session ctx here because cancelling a session
	// while a transcript is mid-type would leave a partial phrase on
	// the user's screen. Closing the session simply drops *future*
	// transcripts via the epoch check.
	daemonCtx context.Context

	mu    sync.Mutex
	mode  sessionMode
	open  bool
	epoch uint64
}

// ErrClipNotImplemented is returned when a clip-mode toggle is
// requested in v1 (phase 9 lights this up).
var ErrClipNotImplemented = fmt.Errorf("clip-mode is not yet implemented (phase 9)")

func newSession(logger *slog.Logger, typer dispatch.Typer, cuer audio.Cuer, asrMon *asrMonitor, vad audio.VAD, daemonCtx context.Context) *session {
	return &session{
		logger:    logger,
		typer:     typer,
		cuer:      cuer,
		asrMon:    asrMon,
		vad:       vad,
		daemonCtx: daemonCtx,
	}
}

// Toggle implements the Pause/Scroll Lock semantics: if a session of
// the requested mode is open, close it; otherwise close any active
// session (D6) and open the requested mode.
func (s *session) Toggle(ctx context.Context, mode string) error {
	target, err := parseSessionMode(mode)
	if err != nil {
		return err
	}
	if target == modeClip {
		return ErrClipNotImplemented
	}

	s.mu.Lock()
	wasOpen := s.open
	wasMode := s.mode
	s.mu.Unlock()

	switch {
	case wasOpen && wasMode == target:
		return s.close(ctx, "toggle")
	case wasOpen && wasMode != target:
		// D6: mutual exclusion. Close the active mode before opening
		// the requested one.
		if err := s.close(ctx, "mode-switch"); err != nil {
			return err
		}
		fallthrough
	default:
		return s.open_(ctx, target)
	}
}

// Snapshot returns the current mode/open pair for status replies.
// Lock-protected so a concurrent Toggle doesn't race the Status read.
func (s *session) Snapshot() (mode string, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode.String(), s.open
}

// open_ transitions the session to (mode, open=true) and fires side
// effects: bump epoch (so any leftover handler from a prior session
// is invalidated), reset VAD calibration, play the open chirp.
func (s *session) open_(ctx context.Context, mode sessionMode) error {
	s.mu.Lock()
	if s.open && s.mode == mode {
		s.mu.Unlock()
		return nil
	}
	s.epoch++
	s.mode = mode
	s.open = true
	epoch := s.epoch
	s.mu.Unlock()

	if s.vad != nil {
		if r, ok := s.vad.(interface{ Reset() }); ok {
			r.Reset()
		}
	}

	s.logger.Info("session.open", "mode", mode.String(), "epoch", epoch)

	if s.cuer != nil {
		if err := s.cuer.Play(ctx, audio.CueOpen); err != nil {
			s.logger.Warn("session.open: cue play failed", "err", err)
		}
	}
	return nil
}

// close transitions the session to (none, open=false). Bumping the
// epoch under the lock invalidates every still-pending utterance
// handler captured by the previous session: when their transcripts
// arrive they will compare epochs and drop instead of typing.
func (s *session) close(ctx context.Context, reason string) error {
	s.mu.Lock()
	if !s.open {
		s.mu.Unlock()
		return nil
	}
	prevMode := s.mode
	s.epoch++
	s.mode = modeNone
	s.open = false
	epoch := s.epoch
	s.mu.Unlock()

	s.logger.Info("session.close", "previous_mode", prevMode.String(), "reason", reason, "epoch", epoch)

	if s.cuer != nil {
		if err := s.cuer.Play(ctx, audio.CueClose); err != nil {
			s.logger.Warn("session.close: cue play failed", "err", err)
		}
	}
	return nil
}

// OnUtterance is the audioMonitor hook. If no session is open, drop.
// Otherwise capture the current epoch and forward to the asrMonitor
// with a transcript callback that re-checks the epoch before typing.
//
// Drops are silent (no log) — a 1-Hz speech burst produces 1+ drops
// per second when the session is closed; logging each one would be
// noise.
func (s *session) OnUtterance(pcm []byte) {
	s.mu.Lock()
	open := s.open
	mode := s.mode
	epoch := s.epoch
	s.mu.Unlock()

	if !open || mode != modeType {
		return
	}

	s.asrMon.OnUtterance(pcm, func(text string) {
		s.mu.Lock()
		current := s.epoch
		stillOpen := s.open && s.mode == modeType
		s.mu.Unlock()
		if !stillOpen || current != epoch {
			s.logger.Info("session.transcript dropped: session changed",
				"submit_epoch", epoch, "current_epoch", current, "text_len", len(text))
			return
		}
		if err := s.typer.Type(s.daemonCtx, text); err != nil {
			s.logger.Warn("session.type dispatch failed", "err", err)
		}
	})
}

func parseSessionMode(s string) (sessionMode, error) {
	switch s {
	case "type":
		return modeType, nil
	case "clip":
		return modeClip, nil
	default:
		return modeNone, fmt.Errorf("unknown session mode %q (want type|clip)", s)
	}
}
