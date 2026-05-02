package whispersup

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config mirrors the [asr.whispercpp] block from §5.2. Validate must be
// called before the supervisor consumes it; production code goes
// through New which calls Validate for you.
type Config struct {
	Binary    string   // path to whisper-server binary
	ModelPath string   // path to ggml-*.bin model file
	Host      string   // listen host; default "127.0.0.1"
	Port      int      // 0 = pick an ephemeral free port at startup
	Threads   int      // 0 = auto: min(NumCPU/2, 8)
	ExtraArgs []string // passthrough flags appended after the typed args
	Env       []string // extra env vars (var=val); appended to os.Environ

	StartupTimeout        time.Duration // max time to wait for first ready; default 30 s
	RestartBackoffInitial time.Duration // first delay after crash; default 500 ms
	RestartBackoffMax     time.Duration // ceiling for exponential backoff; default 30 s

	// BinaryAllowlist gates the binary path; ModelPathAllowlist gates the
	// model path. Both default to safe-prefix lists if left empty (see
	// DefaultBinaryAllowlist and DefaultModelPathAllowlist). Empty lists
	// are populated by withDefaults; an explicit []string{} is treated
	// the same as nil.
	BinaryAllowlist    []string
	ModelPathAllowlist []string
}

// DefaultBinaryAllowlist is the default set of allowed binary-path
// prefixes per §8. Distros install whisper-cpp under one of these.
func DefaultBinaryAllowlist() []string {
	return []string{"/usr/bin", "/usr/local/bin", "/opt"}
}

// DefaultModelPathAllowlist is the default set of allowed model-path
// prefixes. Models live in user data or system-wide library paths,
// not in /tmp or arbitrary user-writable directories.
func DefaultModelPathAllowlist() []string {
	out := []string{"/var/lib/dicta", "/usr/share/dicta"}
	if x := xdgDataHome(); x != "" {
		out = append(out, filepath.Join(x, "dicta"))
	}
	return out
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

// withDefaults returns a copy of c with zero-valued fields populated.
func (c Config) withDefaults() Config {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Threads == 0 {
		c.Threads = autoThreads()
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = 30 * time.Second
	}
	if c.RestartBackoffInitial == 0 {
		c.RestartBackoffInitial = 500 * time.Millisecond
	}
	if c.RestartBackoffMax == 0 {
		c.RestartBackoffMax = 30 * time.Second
	}
	if len(c.BinaryAllowlist) == 0 {
		c.BinaryAllowlist = DefaultBinaryAllowlist()
	}
	if len(c.ModelPathAllowlist) == 0 {
		c.ModelPathAllowlist = DefaultModelPathAllowlist()
	}
	return c
}

// autoThreads implements the "0 = auto" rule from §5.2: half the
// reported CPU count, capped at 8. whisper.cpp inference plateaus
// past ~8 threads on most hardware.
func autoThreads() int {
	return min(max(runtime.NumCPU()/2, 1), 8)
}

// Validate checks that c is well-formed and that all configured paths
// fall under their allowlists. Called by New and exposed for tests.
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.Binary == "" {
		return fmt.Errorf("whispersup: Binary is required")
	}
	if c.ModelPath == "" {
		return fmt.Errorf("whispersup: ModelPath is required")
	}
	if err := pathOnAllowlist(c.Binary, c.BinaryAllowlist); err != nil {
		return fmt.Errorf("whispersup: Binary: %w", err)
	}
	if err := pathOnAllowlist(c.ModelPath, c.ModelPathAllowlist); err != nil {
		return fmt.Errorf("whispersup: ModelPath: %w", err)
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("whispersup: Port out of range: %d", c.Port)
	}
	if c.Threads < 0 {
		return fmt.Errorf("whispersup: Threads must be non-negative: %d", c.Threads)
	}
	for _, e := range c.Env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("whispersup: Env entry %q is not var=val", e)
		}
	}
	return nil
}

// pathOnAllowlist returns nil if path, after Clean, is equal to or
// strictly under one of the allowlist prefixes. Caller must pass an
// already-defaulted allowlist.
func pathOnAllowlist(path string, allowlist []string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
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
		// Strict-under check: path must begin with prefix + "/", so we
		// reject /etc/passwdfile when /etc/passwd is on the allowlist.
		if strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %q not under any allowlist prefix %v", path, allowlist)
}
