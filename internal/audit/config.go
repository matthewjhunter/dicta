package audit

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config mirrors the [audit] block from §5.8 with one user-imposed
// override: KeepAudio defaults to false (the design's example shows
// keep_audio=true, but for v1 we treat WAV capture as the more
// sensitive opt-in and require it to be turned on explicitly via
// --audit-keep-audio).
//
// Field defaults:
//
//	Enabled:        false (overall opt-in; daemon writes nothing without this)
//	Directory:      empty (resolves to $XDG_DATA_HOME/dicta then ~/.local/share/dicta)
//	KeepAudio:      false (extra opt-in for WAV capture)
//	RetentionDays:  0     (forever)
//
// Both Enabled and KeepAudio must be set true to capture audio. Setting
// only KeepAudio without Enabled is a no-op since the writer is
// passthrough.
type Config struct {
	Enabled       bool
	Directory     string
	KeepAudio     bool
	RetentionDays int

	// PathAllowlist gates which directories the writer will create
	// files in. Empty means use DefaultPathAllowlist. §8 mandates
	// path values be validated against allowed prefixes before any
	// filesystem write.
	PathAllowlist []string
}

func (c Config) withDefaults() Config {
	if len(c.PathAllowlist) == 0 {
		c.PathAllowlist = DefaultPathAllowlist()
	}
	return c
}

// DefaultPathAllowlist is the set of prefixes the audit writer is
// permitted to create files under. Mirrors whispersup's data-path
// allowlist (XDG data home + system-wide /var/lib).
func DefaultPathAllowlist() []string {
	out := []string{"/var/lib/dicta"}
	if x := xdgDataHome(); x != "" {
		out = append(out, filepath.Join(x, "dicta"))
	}
	return out
}

// resolveDirectory returns the configured directory if set, else the
// XDG-default. Callers must subsequently validate against the
// allowlist; resolveDirectory is responsible only for filling in the
// blank.
func (c Config) resolveDirectory() (string, error) {
	if c.Directory != "" {
		if !filepath.IsAbs(c.Directory) {
			return "", fmt.Errorf("audit: directory %q must be absolute", c.Directory)
		}
		return filepath.Clean(c.Directory), nil
	}
	x := xdgDataHome()
	if x == "" {
		return "", ErrDirectoryRequired
	}
	return filepath.Join(x, "dicta"), nil
}

func xdgDataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share")
	}
	return ""
}
