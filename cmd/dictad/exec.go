package main

import (
	"errors"
	"fmt"
	"os"
)

// ensureExecutable stats path and confirms it's a regular file (or
// symlink to one) with at least one execute bit set. Symlinks resolve
// transparently via os.Stat.
//
// label is the flag/config name used in the error message so the
// operator can tell which configured binary is missing without grepping
// the path.
//
// The intent is to surface a missing dependency (typical case:
// wl-clipboard or ydotool not installed) at daemon startup, where it
// hits the systemd journal immediately, rather than at first
// invocation, which would be mid-commit for clip-mode.
func ensureExecutable(label, path string) error {
	if path == "" {
		return fmt.Errorf("%s: path is empty", label)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s %q: not found (install the package providing this binary)", label, path)
		}
		return fmt.Errorf("%s %q: stat: %w", label, path, err)
	}
	mode := info.Mode()
	if !mode.IsRegular() {
		return fmt.Errorf("%s %q: not a regular file", label, path)
	}
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("%s %q: not executable (mode %v)", label, path, mode.Perm())
	}
	return nil
}
