package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sweepRetention scans the audit directory for day-subdirectories whose
// name parses as YYYY-MM-DD and deletes those older than
// cfg.RetentionDays. cfg.RetentionDays=0 disables the sweep (keep
// forever).
//
// The "older than" comparison uses cfg.RetentionDays calendar days
// from the current date — i.e. RetentionDays=7 means a directory whose
// date is 8+ days behind today is deleted. The check is on the day
// boundary, not on file mtime, so the sweep is deterministic regardless
// of filesystem clocks or tar/cp games.
//
// Non-day-shaped subdirectories and files at the top level are left
// alone. The sweep deliberately doesn't recurse into dated
// subdirectories: it just rm-rf's the whole day-dir.
func (w *fileWriter) sweepRetention() error {
	if w.cfg.RetentionDays <= 0 {
		return nil
	}
	cutoff := w.now().AddDate(0, 0, -w.cfg.RetentionDays)

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return fmt.Errorf("audit: read %s: %w", w.dir, err)
	}

	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, ok := parseDayDir(e.Name())
		if !ok {
			continue
		}
		if !t.Before(cutoff) {
			continue
		}
		full := filepath.Join(w.dir, e.Name())
		if err := os.RemoveAll(full); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("audit: remove %s: %w", full, err)
		} else if err == nil {
			w.logger.Info("audit.retention pruned day", "dir", full, "age_days", w.cfg.RetentionDays)
		}
	}
	return firstErr
}

// parseDayDir returns the day represented by a "YYYY-MM-DD" directory
// name, or ok=false if the name doesn't match the format. Anything
// that doesn't parse is left untouched on disk — the user might be
// keeping their own subdirectories in there.
func parseDayDir(name string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", name, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
