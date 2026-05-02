package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Clipper writes text into the Wayland clipboard. Per §5.5 the dispatch
// package is dumb wrappers only — the orchestrator decides when to call
// Clip with what text.
//
// Unlike Typer, Clipper does NOT strip newlines: the clip-mode panel
// allows the user to insert literal newlines via Shift+Enter, and
// stripping here would discard intentional formatting.
type Clipper interface {
	Clip(ctx context.Context, text string) error
}

// ClipperConfig parameterizes the SubprocessClipper.
type ClipperConfig struct {
	// Binary is the absolute path to wl-copy. Validated against
	// BinaryAllowlist. Default "/usr/bin/wl-copy".
	Binary string

	// ExtraArgs are passed verbatim to wl-copy after the typed flags.
	// Useful for callers that want --primary or --type forced; the
	// orchestrator should prefer typed flags over a passthrough where
	// the option is in active use.
	ExtraArgs []string

	// InvokeTimeout caps the per-call wait. wl-copy daemonizes after
	// reading stdin so the foreground call returns quickly; the timeout
	// is a safety net for "wl-copy hangs at startup" scenarios. Default 5 s.
	InvokeTimeout time.Duration

	// BinaryAllowlist gates the Binary path. Empty = DefaultBinaryAllowlist.
	BinaryAllowlist []string

	// Logger receives WARN/INFO output. Defaults to slog.Default().
	Logger *slog.Logger
}

func (c ClipperConfig) withDefaults() ClipperConfig {
	if c.Binary == "" {
		c.Binary = "/usr/bin/wl-copy"
	}
	if c.InvokeTimeout == 0 {
		c.InvokeTimeout = 5 * time.Second
	}
	if len(c.BinaryAllowlist) == 0 {
		c.BinaryAllowlist = DefaultBinaryAllowlist()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Validate checks that c is well-formed and that Binary is on the
// allowlist. Called by NewSubprocessClipper.
func (c ClipperConfig) Validate() error {
	c = c.withDefaults()
	if err := pathOnAllowlist(c.Binary, c.BinaryAllowlist); err != nil {
		return fmt.Errorf("dispatch.clipper: Binary: %w", err)
	}
	return nil
}

// SubprocessClipper invokes wl-copy as a subprocess for each Clip call.
// argv is built from typed config; the text payload is delivered on
// stdin so it never touches argv (where shell-escaping bugs lurk).
type SubprocessClipper struct {
	cfg ClipperConfig
}

// NewSubprocessClipper validates cfg and constructs the clipper.
func NewSubprocessClipper(cfg ClipperConfig) (*SubprocessClipper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &SubprocessClipper{cfg: cfg.withDefaults()}, nil
}

// Clip writes text to wl-copy's stdin and waits for the foreground
// process to exit. Empty text is still committed (to clear the
// clipboard or replace it with an empty selection — the orchestrator
// chooses; we don't second-guess).
func (c *SubprocessClipper) Clip(ctx context.Context, text string) error {
	invokeCtx, cancel := context.WithTimeout(ctx, c.cfg.InvokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(invokeCtx, c.cfg.Binary, c.cfg.ExtraArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("wl-copy stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		var pathErr *exec.Error
		if errors.As(err, &pathErr) {
			c.cfg.Logger.Warn("dispatch.clip: wl-copy not executable",
				"binary", c.cfg.Binary, "err", err)
			return fmt.Errorf("wl-copy not executable at %s: %w", c.cfg.Binary, err)
		}
		return fmt.Errorf("wl-copy start: %w", err)
	}

	writeErr := writeAllAndClose(stdin, text)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if writeErr != nil {
		return fmt.Errorf("wl-copy stdin: %w", writeErr)
	}
	if waitErr != nil {
		return fmt.Errorf("wl-copy: %w", waitErr)
	}
	return nil
}

func writeAllAndClose(w io.WriteCloser, s string) error {
	_, werr := w.Write([]byte(s))
	cerr := w.Close()
	switch {
	case werr != nil:
		return werr
	case cerr != nil && !strings.Contains(cerr.Error(), "file already closed"):
		return cerr
	default:
		return nil
	}
}
