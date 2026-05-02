// Package audit writes JSONL session records and optional WAV captures
// with retention managed per config.
//
// The privacy default is OFF: a daemon started without explicitly
// enabling audit writes nothing to disk. Transcript content is sensitive
// (it's literally what the user dictated) and audio is more sensitive
// still. The --audit-enabled flag turns on JSONL recording; an
// additional --audit-keep-audio flag turns on WAV capture. Both default
// to false.
//
// Storage layout, when enabled:
//
//	<directory>/
//	├── 2026-05-02/
//	│   ├── transcripts.jsonl
//	│   └── audio/                     # only if KeepAudio=true
//	│       ├── u1714621234-1.wav
//	│       └── u1714621234-2.wav
//	└── 2026-05-03/
//	    └── ...
//
// Daily rotation. Retention sweep deletes day-directories whose name
// parses as a date older than RetentionDays. RetentionDays=0 means
// keep forever.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Record is one JSONL row. Field tags use snake_case to match the
// design doc's example records and to stay friendly to jq filters.
//
// Only RawText is required; cleaned/audio fields are conditional on the
// session mode and audit config. AudioPath is the relative path
// (relative to the day directory) of the WAV file, populated only when
// KeepAudio=true.
type Record struct {
	Timestamp        time.Time `json:"ts"`
	Mode             string    `json:"mode"`         // "type" | "clip"
	UtteranceID      string    `json:"utterance_id"` // sortable per daemon-run
	Backend          string    `json:"backend,omitempty"`
	RawText          string    `json:"raw_text"`
	CleanedText      string    `json:"cleaned_text,omitempty"`
	Language         string    `json:"language,omitempty"`
	ASRLatencyMs     int64     `json:"asr_latency_ms,omitempty"`
	CleanupLatencyMs int64     `json:"cleanup_latency_ms,omitempty"`
	AudioPath        string    `json:"audio_path,omitempty"`

	// PCM is the raw 16 kHz mono int16 LE audio buffer for the
	// utterance (D15). Carried in-memory only — never serialized into
	// the JSONL row. The fileWriter consumes this to produce the WAV
	// file when KeepAudio=true.
	PCM []byte `json:"-"`
}

// Writer is the audit abstraction phase 11 plumbs through the session.
// Implementations:
//   - passthroughWriter: drops every Record. Used when audit is
//     disabled (the v1 default).
//   - fileWriter: writes JSONL to disk and (optionally) WAV files,
//     with daily rotation and retention.
//
// Record returns nil on success and an error otherwise; the session
// logs failures at WARN level and continues (audit failures must never
// disrupt dictation).
//
// Close flushes any open file handles and runs a final retention sweep.
// It must be safe to call multiple times.
type Writer interface {
	Record(rec Record) error
	Close() error
}

// ErrDirectoryRequired is returned when New is called with Enabled=true
// but no directory could be resolved (config left it empty AND
// XDG_DATA_HOME / $HOME both unavailable).
var ErrDirectoryRequired = errors.New("audit: directory required when enabled=true")

// New returns a Writer per cfg. If cfg.Enabled is false, the returned
// Writer is the passthrough — Record drops every call without touching
// disk. The privacy default ships disabled (§ package doc).
//
// The directory is resolved via cfg.Directory if set, else
// $XDG_DATA_HOME/dicta, else ~/.local/share/dicta. The resolved path
// must fall under cfg.PathAllowlist (defaulted from
// DefaultPathAllowlist when empty); paths outside are rejected per §8.
//
// On startup the retention sweep runs synchronously before the first
// Record call so day-dirs that should already be gone don't accumulate
// stale data.
func New(cfg Config, logger *slog.Logger) (Writer, error) {
	if !cfg.Enabled {
		return passthroughWriter{}, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	dir, err := cfg.resolveDirectory()
	if err != nil {
		return nil, err
	}
	if err := pathOnAllowlist(dir, cfg.PathAllowlist); err != nil {
		return nil, fmt.Errorf("audit: directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	w := &fileWriter{
		cfg:    cfg,
		dir:    dir,
		logger: logger,
		now:    time.Now,
	}
	if err := w.sweepRetention(); err != nil {
		// Sweep failure is logged but doesn't block startup — the daemon
		// must still come up. A persistent FS issue will surface again
		// on the next Record call.
		logger.Warn("audit.sweep_retention failed", "err", err)
	}
	return w, nil
}

// Passthrough returns the no-op Writer. Useful for tests or for code
// paths that explicitly want audit disabled without going through New.
func Passthrough() Writer { return passthroughWriter{} }

type passthroughWriter struct{}

func (passthroughWriter) Record(_ Record) error { return nil }
func (passthroughWriter) Close() error          { return nil }

// fileWriter implements the on-disk JSONL + WAV writer. Concurrent
// Record calls are serialized through mu so JSONL lines don't
// interleave; WAV writes are independent files, but the directory
// creation inside writeWAV is also covered by mu to keep the
// MkdirAll/Open sequence atomic.
type fileWriter struct {
	cfg    Config
	dir    string
	logger *slog.Logger

	// now is overridable for tests. In production this is time.Now.
	now func() time.Time

	mu       sync.Mutex
	jsonlDay string   // YYYY-MM-DD of the currently open jsonl
	jsonl    *os.File // currently open transcripts.jsonl, or nil
	closed   bool
}

func (w *fileWriter) Record(rec Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("audit: writer closed")
	}

	if rec.Timestamp.IsZero() {
		rec.Timestamp = w.now()
	}
	day := rec.Timestamp.Format("2006-01-02")
	dayDir := filepath.Join(w.dir, day)

	// Lazily rotate or create the JSONL file for this day.
	if w.jsonl == nil || w.jsonlDay != day {
		if w.jsonl != nil {
			_ = w.jsonl.Close()
			w.jsonl = nil
		}
		if err := os.MkdirAll(dayDir, 0o700); err != nil {
			return fmt.Errorf("audit: mkdir %s: %w", dayDir, err)
		}
		jpath := filepath.Join(dayDir, "transcripts.jsonl")
		f, err := os.OpenFile(jpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("audit: open %s: %w", jpath, err)
		}
		w.jsonl = f
		w.jsonlDay = day
	}

	// Write the WAV first (if requested) so we can populate AudioPath
	// in the JSONL row before serialization.
	if w.cfg.KeepAudio && len(rec.PCM) > 0 && rec.UtteranceID != "" {
		audioRel := filepath.Join("audio", sanitizeID(rec.UtteranceID)+".wav")
		audioAbs := filepath.Join(dayDir, audioRel)
		if err := writeWAVFile(audioAbs, rec.PCM); err != nil {
			// Don't fail the whole record — JSONL still has value
			// without the audio. Log and continue with empty AudioPath.
			w.logger.Warn("audit.wav_write failed", "err", err, "path", audioAbs)
		} else {
			rec.AudioPath = audioRel
		}
	}

	enc := json.NewEncoder(w.jsonl)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&rec); err != nil {
		return fmt.Errorf("audit: encode jsonl: %w", err)
	}
	return nil
}

func (w *fileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.jsonl != nil {
		err = w.jsonl.Close()
		w.jsonl = nil
	}
	if sweepErr := w.sweepRetention(); sweepErr != nil && err == nil {
		err = sweepErr
	}
	return err
}

// sanitizeID strips path-traversal characters from utterance IDs so a
// malicious or misbehaving ASR backend can't redirect WAV writes
// outside the audio/ subdirectory. utterance IDs are daemon-generated
// (asrMonitor), so this is belt-and-braces — paranoia is cheap.
func sanitizeID(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "\\", "_")
	id = strings.ReplaceAll(id, "..", "_")
	id = strings.TrimSpace(id)
	if id == "" {
		return "unknown"
	}
	return id
}

// pathOnAllowlist mirrors the helper in internal/whispersup. Validating
// that the resolved directory falls under a known prefix prevents a
// misconfigured config.toml from writing audit data into /etc or
// somewhere equally surprising.
func pathOnAllowlist(path string, allowlist []string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	for _, prefix := range allowlist {
		prefix = filepath.Clean(prefix)
		if clean == prefix {
			return nil
		}
		if strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %q not under any allowlist prefix %v", path, allowlist)
}

// SweepLoop runs sweepRetention every interval until ctx is cancelled.
// Useful for long-running daemons that don't restart often enough for
// the once-at-startup sweep to keep retention current. interval=0
// disables the loop (returns immediately).
func (w *fileWriter) SweepLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.sweepRetention(); err != nil {
				w.logger.Warn("audit.sweep_retention failed", "err", err)
			}
		}
	}
}
