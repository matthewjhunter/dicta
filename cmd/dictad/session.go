package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/control"
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
	logger  *slog.Logger
	typer   dispatch.Typer
	clipper dispatch.Clipper // optional; required for clip-mode commit
	cuer    audio.Cuer
	asrMon  *asrMonitor
	vad     audio.VAD         // optional: Reset() called on session-open if non-nil
	bus     *eventBus         // optional; nil = no event publish
	preview previewController // optional; required for clip-mode

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

// ErrClipNotConfigured is returned when a clip-mode toggle fires but
// the daemon was not started with the clipper + preview wiring (e.g.
// no --preview-binary flag). The control reply codes "not_implemented"
// to match the wire shape of features that are absent vs misconfigured.
var ErrClipNotConfigured = fmt.Errorf("clip-mode requires --preview-binary and --wl-copy-binary")

// ErrCommitOnlyValidInClipMode is returned by Commit when called outside
// a live clip-mode session. The clip-mode panel is the only client
// authorized to issue a commit (§5.6: commit.text is panel-edited).
var ErrCommitOnlyValidInClipMode = fmt.Errorf("commit only valid while clip-mode session is open")

// ErrCancelOnlyValidInClipMode mirrors ErrCommitOnlyValidInClipMode
// for the cancel command.
var ErrCancelOnlyValidInClipMode = fmt.Errorf("cancel only valid while clip-mode session is open")

func newSession(logger *slog.Logger, typer dispatch.Typer, clipper dispatch.Clipper, cuer audio.Cuer, asrMon *asrMonitor, vad audio.VAD, bus *eventBus, preview previewController, daemonCtx context.Context) *session {
	s := &session{
		logger:    logger,
		typer:     typer,
		clipper:   clipper,
		cuer:      cuer,
		asrMon:    asrMon,
		vad:       vad,
		bus:       bus,
		preview:   preview,
		daemonCtx: daemonCtx,
	}
	if preview != nil {
		// When the panel exits on its own (after sending commit/cancel,
		// or after window-close), close the session so state stays in
		// sync. The handler's commit/cancel path will have already
		// closed by the time onExit fires in the normal flow; the
		// idempotent close handles the race.
		preview.OnExit(func() {
			_ = s.close(daemonCtx, "panel-exited")
		})
	}
	return s
}

// Toggle implements the Pause/Scroll Lock semantics: if a session of
// the requested mode is open, close it; otherwise close any active
// session (D6) and open the requested mode.
func (s *session) Toggle(ctx context.Context, mode string) error {
	target, err := parseSessionMode(mode)
	if err != nil {
		return err
	}
	if target == modeClip && (s.preview == nil || s.clipper == nil) {
		return ErrClipNotConfigured
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
// is invalidated), reset VAD calibration, play the open chirp, and
// for clip-mode spawn the preview subprocess.
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

	if mode == modeClip && s.preview != nil {
		// Spawn before publishing state and playing cue so a spawn
		// failure rolls the session back to closed without a confusing
		// half-open emission.
		if err := s.preview.Spawn(s.daemonCtx); err != nil {
			s.mu.Lock()
			s.epoch++
			s.mode = modeNone
			s.open = false
			s.mu.Unlock()
			s.logger.Warn("session.open: preview spawn failed", "err", err)
			return fmt.Errorf("preview spawn: %w", err)
		}
	}

	if s.vad != nil {
		if r, ok := s.vad.(interface{ Reset() }); ok {
			r.Reset()
		}
	}

	s.logger.Info("session.open", "mode", mode.String(), "epoch", epoch)
	s.publishState(mode, true)

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
//
// On clip-mode close, the preview subprocess is killed via SIGTERM.
// Kill is idempotent — if the panel already exited (after sending
// commit/cancel), the call is a no-op.
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

	if prevMode == modeClip && s.preview != nil {
		if err := s.preview.Kill(); err != nil {
			s.logger.Warn("session.close: preview kill failed", "err", err)
		}
	}

	s.logger.Info("session.close", "previous_mode", prevMode.String(), "reason", reason, "epoch", epoch)
	s.publishState(modeNone, false)

	if s.cuer != nil {
		if err := s.cuer.Play(ctx, audio.CueClose); err != nil {
			s.logger.Warn("session.close: cue play failed", "err", err)
		}
	}
	return nil
}

// Commit handles the panel's {"cmd":"commit","text":...} message. The
// text is the panel's edited buffer (authoritative per §5.6) and is
// sent verbatim to wl-copy. The session closes whether the clipper
// succeeded or failed — a clipboard failure is surfaced to the panel
// but doesn't leave clip-mode hanging.
func (s *session) Commit(ctx context.Context, text string) error {
	s.mu.Lock()
	open := s.open
	mode := s.mode
	s.mu.Unlock()
	if !open || mode != modeClip {
		return ErrCommitOnlyValidInClipMode
	}

	clipErr := s.clipper.Clip(ctx, text)
	if clipErr != nil {
		s.logger.Warn("session.commit: clipper failed", "err", clipErr, "text_len", len(text))
	} else {
		s.logger.Info("session.commit", "text_len", len(text))
	}

	if err := s.close(ctx, "commit"); err != nil {
		s.logger.Warn("session.commit: close failed", "err", err)
	}
	return clipErr
}

// Cancel handles the panel's {"cmd":"cancel"} message. The buffer is
// discarded and clip-mode closes without dispatching anything.
func (s *session) Cancel(ctx context.Context) error {
	s.mu.Lock()
	open := s.open
	mode := s.mode
	s.mu.Unlock()
	if !open || mode != modeClip {
		return ErrCancelOnlyValidInClipMode
	}
	s.logger.Info("session.cancel")
	return s.close(ctx, "cancel")
}

// publishState emits a session_state event on the bus. Calls without a
// bus configured are silent no-ops, which keeps tests that don't care
// about event plumbing simple.
func (s *session) publishState(mode sessionMode, open bool) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(control.Event{
		Event: "session_state",
		Data:  control.SessionStateData{Mode: mode.String(), Open: open},
	})
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
