package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// previewController is the interface session calls when clip-mode opens
// or closes. Spawn must be idempotent (a second Spawn while already
// running returns errAlreadySpawned), and Kill must tolerate
// already-dead processes.
//
// onExit, if set, is invoked when the panel subprocess exits without
// the daemon having called Kill — i.e. the panel terminated on its
// own (after sending commit/cancel, or after the user closed the
// window outside of the documented key handlers). The session uses it
// to close clip-mode automatically when the panel goes away.
type previewController interface {
	Spawn(ctx context.Context) error
	Kill() error
	OnExit(fn func())
}

// previewProc spawns and tracks the cmd/dicta-preview subprocess. It
// applies the same Setpgid + cmd.Cancel kill-group pattern as
// internal/audio.SubprocessCapture so a misbehaving panel that fork()s
// its own children still gets cleaned up on session close.
type previewProc struct {
	binary    string
	socket    string
	extraArgs []string

	binaryAllowlist []string
	logger          *slog.Logger
	killGrace       time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	exitCh  chan struct{}
	onExit  func()
	stopped bool // true after Kill so the exit watcher knows not to fire onExit
}

type previewConfig struct {
	Binary          string
	Socket          string
	ExtraArgs       []string
	BinaryAllowlist []string
	KillGrace       time.Duration
	Logger          *slog.Logger
}

// errAlreadySpawned is returned when a second Spawn fires while the
// previous panel is still running. Caller's responsibility to Kill
// before re-Spawn (or treat the no-op as success).
var errAlreadySpawned = errors.New("preview: already spawned")

// newPreviewProc validates cfg and constructs the controller. The
// binary path is checked against the allowlist immediately; spawn-time
// failures (binary missing, no exec perms) surface from Spawn.
func newPreviewProc(cfg previewConfig) (*previewProc, error) {
	if cfg.Binary == "" {
		return nil, fmt.Errorf("preview: Binary is required")
	}
	allowlist := cfg.BinaryAllowlist
	if len(allowlist) == 0 {
		allowlist = defaultPreviewAllowlist()
	}
	if err := previewPathOnAllowlist(cfg.Binary, allowlist); err != nil {
		return nil, fmt.Errorf("preview: Binary: %w", err)
	}
	if cfg.Logger == nil {
		return nil, fmt.Errorf("preview: Logger is required")
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 2 * time.Second
	}
	return &previewProc{
		binary:          cfg.Binary,
		socket:          cfg.Socket,
		extraArgs:       cfg.ExtraArgs,
		binaryAllowlist: allowlist,
		logger:          cfg.Logger,
		killGrace:       cfg.KillGrace,
	}, nil
}

// Spawn launches the preview subprocess. ctx is the daemon's parent
// context — when ctx is canceled (daemon shutdown), the panel is
// killed via the Setpgid group.
func (p *previewProc) Spawn(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		return errAlreadySpawned
	}
	p.stopped = false

	args := []string{"--socket", p.socket}
	args = append(args, p.extraArgs...)

	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, p.binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = p.killGrace

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("preview: start %s: %w", p.binary, err)
	}

	p.cmd = cmd
	p.cancel = cancel
	p.exitCh = make(chan struct{})
	exitCh := p.exitCh
	pid := cmd.Process.Pid

	p.logger.Info("preview.spawn", "binary", p.binary, "pid", pid)

	go func() {
		err := cmd.Wait()
		// Mark exited under the lock before invoking onExit so the
		// session can call Spawn again immediately.
		p.mu.Lock()
		p.cmd = nil
		p.cancel = nil
		p.exitCh = nil
		stopped := p.stopped
		onExit := p.onExit
		p.mu.Unlock()
		close(exitCh)

		switch {
		case stopped:
			p.logger.Info("preview.exit (after Kill)", "pid", pid, "err", err)
		case err != nil:
			p.logger.Info("preview.exit (unexpected)", "pid", pid, "err", err)
			if onExit != nil {
				onExit()
			}
		default:
			p.logger.Info("preview.exit (clean)", "pid", pid)
			if onExit != nil {
				onExit()
			}
		}
	}()

	return nil
}

// Kill sends SIGTERM via the panel's process group and waits for the
// subprocess to exit (bounded by KillGrace; CommandContext.WaitDelay
// upgrades to SIGKILL if it hangs). Returns nil if no panel is running.
func (p *previewProc) Kill() error {
	p.mu.Lock()
	cmd := p.cmd
	cancel := p.cancel
	exitCh := p.exitCh
	p.stopped = true
	p.mu.Unlock()

	if cmd == nil {
		return nil
	}
	if cancel != nil {
		cancel() // triggers cmd.Cancel → SIGTERM to process group
	}
	if exitCh != nil {
		select {
		case <-exitCh:
		case <-time.After(p.killGrace + time.Second):
			// CommandContext should have escalated to SIGKILL by now;
			// log and move on rather than block the caller forever.
			p.logger.Warn("preview.kill: process did not exit within grace+1s")
		}
	}
	return nil
}

// OnExit registers a callback fired when the subprocess exits without
// a Kill having been called (i.e. the panel exited on its own). The
// session uses this to close clip-mode automatically.
func (p *previewProc) OnExit(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onExit = fn
}

// defaultPreviewAllowlist is the set of allowed prefixes for the
// dicta-preview binary. System paths are listed for distro-packaged
// installs; $HOME/.local/bin is included because `task install:user`
// (the documented user-install path) places the binary there alongside
// dictad and dicta.
func defaultPreviewAllowlist() []string {
	allow := []string{"/usr/bin", "/usr/local/bin", "/opt"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		allow = append(allow, filepath.Join(home, ".local", "bin"))
	}
	return allow
}

func previewPathOnAllowlist(path string, allowlist []string) error {
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
		if strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %q not under any allowlist prefix %v", path, allowlist)
}
