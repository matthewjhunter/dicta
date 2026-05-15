package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/audit"
	"github.com/matthewjhunter/dicta/internal/cleanup"
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
	cleaner cleanup.Cleaner   // optional; nil = passthrough for both modes
	auditW  audit.Writer      // optional; nil = no audit

	// flushAudio asks the audio capture/VAD layer to finalize and emit
	// any in-flight utterance immediately. Called from close() before
	// open is set to false so the flushed utterance still passes the
	// OnUtterance open-gate and reaches the type queue. Optional —
	// tests and clip-only deployments leave it nil.
	flushAudio func()

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

	// typeQueue carries type-mode jobs to a single worker goroutine
	// that dispatches them to ydotool in submission order. A mutex
	// alone wouldn't suffice: asrMonitor.MaxConcurrent allows multiple
	// transcribe goroutines, and Whisper latency varies per utterance,
	// so a small fast utterance can complete *and acquire a mutex*
	// before a large slow one that was submitted earlier — producing
	// out-of-order typing. A FIFO channel preserves submission order;
	// running a single worker also rules out two ydotool subprocesses
	// racing events into uinput (which produced "gibberish in the
	// middle" character-interleaving in earlier sessions).
	//
	// Buffered to absorb bursts where typing falls behind transcription
	// (long ydotool dispatch + several short utterances back-to-back).
	// If even this fills up, the OnUtterance callback drops with a
	// WARN — better than scrambled output.
	typeQueue      chan *typeJob
	typeShutdown   sync.Once
	typeWorkerDone chan struct{}
}

// typeJob is one unit of work for the type-mode worker. It is
// allocated and enqueued at OnUtterance entry, BEFORE the asrMonitor
// transcribe goroutine starts, so the queue order matches submission
// order even when transcribes complete out of order under load. The
// asrMonitor callbacks (onTranscript on success, onSkip on
// drop/error/filter) populate text and close ready; the worker
// blocks on ready before deciding whether to type.
//
// text is empty if the utterance was skipped — concurrency cap,
// transcribe error, hallucination/repetition filter, or session
// epoch drift between submission and transcript callback.
type typeJob struct {
	epoch uint64
	text  string
	ready chan struct{}
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

func newSession(logger *slog.Logger, typer dispatch.Typer, clipper dispatch.Clipper, cuer audio.Cuer, asrMon *asrMonitor, vad audio.VAD, bus *eventBus, preview previewController, cleaner cleanup.Cleaner, auditW audit.Writer, flushAudio func(), daemonCtx context.Context) *session {
	if cleaner == nil {
		cleaner = cleanup.Passthrough()
	}
	if auditW == nil {
		auditW = audit.Passthrough()
	}
	s := &session{
		logger:     logger,
		typer:      typer,
		clipper:    clipper,
		cuer:       cuer,
		asrMon:     asrMon,
		vad:        vad,
		bus:        bus,
		preview:    preview,
		cleaner:    cleaner,
		auditW:     auditW,
		flushAudio: flushAudio,
		daemonCtx:  daemonCtx,
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

	// Start the type-mode dispatch worker. One per session-instance.
	// Buffer size 8: enough headroom for ~8 short utterances stacking
	// up while a long one is mid-type. Beyond that the OnUtterance
	// callback will drop with a WARN — backpressure surfaced to the
	// user as a visible "queue full" log rather than scrambled output.
	s.typeQueue = make(chan *typeJob, 8)
	s.typeWorkerDone = make(chan struct{})
	go s.typeWorker()
	return s
}

// typeWorker dispatches type-mode jobs in submission order. Exits when
// daemonCtx is cancelled or typeQueue is closed (whichever comes
// first). The worker tracks typedInEpoch privately — the leading-space
// decision for every utterance after the first in a session lives
// inside this single goroutine, so there's no shared-state race
// between the read and the increment that gates it.
func (s *session) typeWorker() {
	defer close(s.typeWorkerDone)
	var currentEpoch uint64
	var initialized bool
	var typedInEpoch uint64
	for {
		select {
		case <-s.daemonCtx.Done():
			return
		case job, ok := <-s.typeQueue:
			if !ok {
				return
			}
			// Reset per-epoch typed counter when we cross an epoch
			// boundary. Done before the readiness wait so the counter
			// state matches the job's epoch even if we end up
			// skipping (no leading space credit).
			if !initialized || job.epoch != currentEpoch {
				currentEpoch = job.epoch
				typedInEpoch = 0
				initialized = true
			}
			// Wait for the asrMonitor to finish with this utterance.
			// onTranscript fills text + closes ready; onSkip just
			// closes ready with text empty. Daemon shutdown is the
			// only way out short of the asrMon resolving.
			select {
			case <-s.daemonCtx.Done():
				return
			case <-job.ready:
			}
			if job.text == "" {
				continue
			}
			// Re-check the session epoch: if the session has been
			// re-opened (or the mode has switched) since the job was
			// submitted, the transcript belongs to a session the user
			// no longer considers active. Drop without typing.
			//
			// Note: we deliberately do NOT check `open` here. A close
			// without a subsequent re-open leaves the epoch unchanged,
			// so anything queued at close time drains. This matches
			// the user-visible contract: "tapping mute / pressing
			// Pause stops me from accepting more audio, but commits
			// what I already said."
			s.mu.Lock()
			liveEpoch := s.epoch
			s.mu.Unlock()
			if liveEpoch != job.epoch {
				continue
			}
			text := job.text
			if typedInEpoch > 0 {
				text = " " + text
			}
			if err := s.typer.Type(s.daemonCtx, text); err != nil {
				s.logger.Warn("session.type dispatch failed", "err", err)
				continue
			}
			typedInEpoch++
		}
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

// EnsureTypeOpen is the unmute-to-dictate hook: open type-mode if no
// session is currently active. If a session is already open in either
// mode the call is a no-op — an explicit clip-mode session must not
// be interrupted by an inferred mic-state event.
//
// Returns nil on no-op and on successful open.
func (s *session) EnsureTypeOpen(ctx context.Context) error {
	s.mu.Lock()
	open := s.open
	s.mu.Unlock()
	if open {
		return nil
	}
	return s.open_(ctx, modeType)
}

// CloseIfTypeOpen is the mic-muted hook: close the session iff it is
// currently open in type-mode. A clip-mode session is left alone (see
// EnsureTypeOpen rationale); a closed session is a no-op.
func (s *session) CloseIfTypeOpen(ctx context.Context) error {
	s.mu.Lock()
	open := s.open
	mode := s.mode
	s.mu.Unlock()
	if !open || mode != modeType {
		return nil
	}
	return s.close(ctx, "mute")
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

// close transitions the session to (none, open=false). Closing does
// NOT bump the epoch: queued transcripts whose ASR work was already
// in flight when the user pressed Pause / tapped mute are allowed
// to type to completion. The epoch is bumped on the NEXT open, which
// is the canonical "you're stale, drop" boundary — anything from the
// closed session that arrives after a re-open compares epochs and
// drops. This means the worker can drain its queue after a close
// without typing into a different session's context.
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
	epoch := s.epoch
	s.mu.Unlock()

	// Flush any in-flight VAD utterance BEFORE flipping open=false, so
	// the flushed OnUtterance call sees open=true and enqueues. Without
	// this, a user who taps mute right after speaking has their last
	// utterance dropped: mute-debounce (~250 ms) closes the session
	// well before the VAD hangover (default 800 ms) finalizes, and
	// OnUtterance's !open gate then silently discards the buffered
	// audio. flushAudio is synchronous: it blocks until the audio
	// loop has emitted (and the resulting OnUtterance callback has
	// returned, which is what enqueues the job).
	if s.flushAudio != nil {
		s.flushAudio()
	}

	s.mu.Lock()
	s.mode = modeNone
	s.open = false
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

// Shutdown is the SIGTERM hook (§12 design doc): if a session is open
// at daemon shutdown, close it explicitly so the close-cue plays, the
// audit/event log records the close, and (for clip-mode) the preview
// panel gets an explicit Kill on top of the ctx-cancellation that
// already fired. Idempotent: a closed session is a no-op.
func (s *session) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	open := s.open
	s.mu.Unlock()

	var err error
	if open {
		s.logger.Info("session.shutdown: closing open session before exit")
		err = s.close(ctx, "shutdown")
	}

	// Stop the typing worker. Idempotent — sync.Once guards repeat
	// Shutdown calls (the test suite exercises that path).
	s.typeShutdown.Do(func() {
		close(s.typeQueue)
	})
	<-s.typeWorkerDone
	return err
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
// with a transcript callback that re-checks the epoch before acting.
//
// Type-mode: raw transcript → publish raw event → ydotool. Cleanup is
// not invoked (D12: cleanup runs on clip-mode text only).
//
// Clip-mode: raw transcript → mechanical cleanup → publish cleaned
// event. The panel subscribes to "transcript" events and renders the
// cleaned text in its editable buffer; nothing reaches the clipboard
// until the user presses Enter inside the panel. Cleanup errors fall
// back to the raw transcript with a WARN — losing punctuation polish
// is preferable to losing the utterance entirely.
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

	if !open || (mode != modeType && mode != modeClip) {
		return
	}

	// In type-mode, pre-allocate the typing job and enqueue it BEFORE
	// submitting to the asrMonitor. This is what guarantees typing
	// happens in submission order even when transcribe goroutines
	// (running in parallel up to MaxConcurrent) complete out of order
	// — e.g. a short, fast-Whisper utterance that follows a long,
	// slow-Whisper one would otherwise type first under a plain mutex.
	// The typeJob's ready channel is closed by exactly one of the
	// asrMonitor callbacks below (onTranscript on success, onSkip on
	// drop / error / filter), unblocking the worker.
	var job *typeJob
	if mode == modeType {
		job = &typeJob{epoch: epoch, ready: make(chan struct{})}
		select {
		case s.typeQueue <- job:
		default:
			s.logger.Warn("session.type queue full; dropping utterance",
				"audio_bytes", len(pcm))
			return
		}
	}

	s.asrMon.OnUtterance(pcm,
		func(tr transcriptResult) {
			// Drop if the session has been re-opened (epoch advanced)
			// or switched to a different mode since submission. A
			// close *without* a re-open leaves the epoch alone and
			// lets the transcript drain to the worker — see the
			// session.close comment for the drain contract.
			s.mu.Lock()
			current := s.epoch
			currentMode := s.mode
			s.mu.Unlock()
			if current != epoch || (currentMode != mode && currentMode != modeNone) {
				s.logger.Info("session.transcript dropped: session changed",
					"submit_epoch", epoch, "current_epoch", current, "text_len", len(tr.Text))
				if job != nil {
					close(job.ready)
				}
				return
			}

			var cleanupLatencyMs int64
			var cleaned string
			switch mode {
			case modeType:
				cleaned = tr.Text
				s.publishTranscript(tr, tr.Text)
				job.text = tr.Text
				close(job.ready)
			case modeClip:
				cleaned, cleanupLatencyMs = s.runCleanup(tr.Text)
				s.publishTranscript(tr, cleaned)
			}

			s.recordAudit(mode, tr, cleaned, cleanupLatencyMs)
		},
		func() {
			// onSkip fires for asrMon-side drops (concurrency cap,
			// transcribe error, hallucination/repetition filter). For
			// type-mode that means the queued job will never get
			// text — close ready so the worker advances past it.
			if job != nil {
				close(job.ready)
			}
		})
}

// runCleanup runs the mechanical profile on raw and returns the cleaned
// text along with the wall-clock latency in milliseconds. The cleanup
// call is bounded by the cleaner's own timeout (cleanup.Config.Timeout).
// On error we fall back to the raw transcript with a WARN: the panel
// should still see *something* even if the cleanup endpoint is down,
// and the user can fix punctuation by hand before pressing Enter.
func (s *session) runCleanup(raw string) (string, int64) {
	cleanCtx, cancel := context.WithCancel(s.daemonCtx)
	defer cancel()
	start := time.Now()
	cleaned, err := s.cleaner.Clean(cleanCtx, raw, cleanup.ProfileMechanical)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		s.logger.Warn("session.cleanup failed; falling back to raw transcript",
			"err", err, "raw_len", len(raw))
		return raw, latency
	}
	return cleaned, latency
}

// recordAudit emits one audit Record per utterance. Failures are
// logged at WARN level and ignored — audit must never disrupt
// dictation. Passthrough writers no-op.
func (s *session) recordAudit(mode sessionMode, tr transcriptResult, cleaned string, cleanupMs int64) {
	if s.auditW == nil {
		return
	}
	rec := audit.Record{
		Timestamp:        time.Now(),
		Mode:             mode.String(),
		UtteranceID:      tr.UtteranceID,
		Backend:          tr.Backend,
		RawText:          tr.Text,
		CleanedText:      cleaned,
		Language:         tr.Language,
		ASRLatencyMs:     tr.ASRLatencyMs,
		CleanupLatencyMs: cleanupMs,
		PCM:              tr.PCM,
	}
	if err := s.auditW.Record(rec); err != nil {
		s.logger.Warn("session.audit_record failed", "err", err, "utterance_id", tr.UtteranceID)
	}
}

// publishTranscript emits a transcript event to the bus. The text
// argument is the version that should reach subscribers (raw for type-
// mode, cleaned for clip-mode). The utteranceID and language ride
// through unchanged so subscribers can correlate.
func (s *session) publishTranscript(tr transcriptResult, text string) {
	if s.bus == nil || text == "" {
		return
	}
	s.bus.Publish(control.Event{
		Event: "transcript",
		Data: control.TranscriptData{
			Text:        text,
			Final:       true,
			UtteranceID: tr.UtteranceID,
			Language:    tr.Language,
		},
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
