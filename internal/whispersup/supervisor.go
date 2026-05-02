package whispersup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// State enumerates the supervisor's lifecycle phases.
type State int

const (
	StateIdle     State = iota // before Start
	StateStarting              // subprocess spawned, awaiting first ready
	StateReady                 // subprocess is up and probe succeeded
	StateCrashed               // subprocess exited; backoff before respawn
	StateStopping              // Stop requested; tearing down
	StateStopped               // Stop completed
)

// String returns a stable lower-case label suitable for status output.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateCrashed:
		return "crashed"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Supervisor runs the whisper-server lifecycle. One Supervisor per
// dictad process; not safe for concurrent re-initialization, but all
// query methods (State, Endpoint, WaitReady) are safe to call from any
// goroutine.
type Supervisor struct {
	cfg    Config
	logger *slog.Logger
	probe  Probe

	mu          sync.Mutex
	state       State
	port        int
	endpoint    string
	startedOnce bool
	stopOnce    sync.Once
	stopCh      chan struct{}
	doneCh      chan struct{}
	readyCh     chan struct{} // closed on first transition into ready
	lastErr     error
	restarts    int
}

// New returns a Supervisor with cfg validated and defaults applied.
// logger is required; pass slog.Default() if nothing better.
func New(cfg Config, logger *slog.Logger) (*Supervisor, error) {
	if logger == nil {
		return nil, errors.New("whispersup: logger is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Supervisor{
		cfg:     cfg.withDefaults(),
		logger:  logger,
		probe:   TCPConnectProbe,
		state:   StateIdle,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		readyCh: make(chan struct{}),
	}, nil
}

// SetProbe overrides the readiness probe. Tests use this; production
// leaves it on the default. Must be called before Start.
func (s *Supervisor) SetProbe(p Probe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p != nil {
		s.probe = p
	}
}

// State returns the current lifecycle state.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Endpoint returns the OpenAI-compatible transcription URL once the
// subprocess is ready. Empty before that.
func (s *Supervisor) Endpoint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoint
}

// LastError returns the last error observed by the supervisor (probe
// failure, subprocess exit error, etc.). Cleared after a successful
// ready transition.
func (s *Supervisor) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Restarts returns the count of subprocess restart attempts since
// the supervisor started.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// Start spawns the supervise loop. It returns immediately; use
// WaitReady to gate ASR availability.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.startedOnce {
		s.mu.Unlock()
		return errors.New("whispersup: Start called twice")
	}
	s.startedOnce = true
	s.state = StateStarting
	if s.cfg.Port != 0 {
		s.port = s.cfg.Port
		s.endpoint = endpointURL(s.cfg.Host, s.port)
	}
	s.mu.Unlock()

	go s.run(ctx)
	return nil
}

// WaitReady blocks until the supervisor reaches StateReady at least
// once or ctx ends. Subsequent calls return immediately if already
// ready.
func (s *Supervisor) WaitReady(ctx context.Context) error {
	select {
	case <-s.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.doneCh:
		return errors.New("whispersup: supervisor stopped before ready")
	}
}

// Stop signals the supervise loop to tear down the subprocess and
// exit. Idempotent and safe to call from any goroutine.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

// run is the supervise loop: spawn, probe, wait-for-exit, backoff,
// respawn. Exits on stopCh.
func (s *Supervisor) run(ctx context.Context) {
	defer close(s.doneCh)
	defer s.setState(StateStopped)

	backoff := s.cfg.RestartBackoffInitial
	for {
		if s.shouldStop(ctx) {
			return
		}

		port, err := s.resolvePort()
		if err != nil {
			s.recordError(fmt.Errorf("resolve port: %w", err))
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.cfg.RestartBackoffMax)
			continue
		}

		cmd := s.buildCmd(ctx, port)
		if err := cmd.Start(); err != nil {
			s.recordError(fmt.Errorf("start %s: %w", s.cfg.Binary, err))
			s.setState(StateCrashed)
			s.incRestart()
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.cfg.RestartBackoffMax)
			continue
		}

		s.logger.Info("whispersup.spawn",
			"binary", s.cfg.Binary,
			"port", port,
			"pid", cmd.Process.Pid,
		)

		probeCtx, cancelProbe := context.WithTimeout(ctx, s.cfg.StartupTimeout)
		probeErr := s.probe(probeCtx, s.cfg.Host, port)
		cancelProbe()

		if probeErr != nil {
			s.logger.Warn("whispersup.probe_failed", "err", probeErr, "pid", cmd.Process.Pid)
			s.recordError(fmt.Errorf("readiness probe: %w", probeErr))
			s.killGroup(cmd)
			_ = cmd.Wait()
			s.setState(StateCrashed)
			s.incRestart()
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.cfg.RestartBackoffMax)
			continue
		}

		s.markReady(port)
		backoff = s.cfg.RestartBackoffInitial // reset on success

		// Wait for either subprocess exit or stop signal.
		exitCh := make(chan error, 1)
		go func() { exitCh <- cmd.Wait() }()

		select {
		case <-s.stopCh:
			s.setState(StateStopping)
			s.killGroup(cmd)
			<-exitCh
			return
		case <-ctx.Done():
			s.setState(StateStopping)
			s.killGroup(cmd)
			<-exitCh
			return
		case err := <-exitCh:
			s.logger.Warn("whispersup.exit", "err", err, "pid", cmd.Process.Pid)
			s.recordError(fmt.Errorf("subprocess exited: %w", err))
			s.setState(StateCrashed)
			s.incRestart()
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.cfg.RestartBackoffMax)
		}
	}
}

// resolvePort returns the port the next subprocess should bind to.
// If Port is non-zero, returns it unchanged; if Port is 0, picks a
// free ephemeral port the first time and reuses it across restarts
// so the asrclient/whispercpp.Client's endpoint URL stays stable.
func (s *Supervisor) resolvePort() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.port != 0 {
		return s.port, nil
	}
	port, err := pickFreePort(s.cfg.Host)
	if err != nil {
		return 0, err
	}
	s.port = port
	s.endpoint = endpointURL(s.cfg.Host, port)
	return port, nil
}

func (s *Supervisor) buildCmd(ctx context.Context, port int) *exec.Cmd {
	args := []string{
		"-m", s.cfg.ModelPath,
		"--host", s.cfg.Host,
		"--port", strconv.Itoa(port),
		"-t", strconv.Itoa(s.cfg.Threads),
	}
	args = append(args, s.cfg.ExtraArgs...)
	cmd := exec.CommandContext(ctx, s.cfg.Binary, args...)
	cmd.Stderr = nil // phase-11 audit will capture this
	cmd.Stdout = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	if len(s.cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), s.cfg.Env...)
	}
	return cmd
}

func (s *Supervisor) killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	// SIGTERM gives whisper-server a chance to flush; the WaitDelay on
	// cmd will SIGKILL if it doesn't exit promptly.
}

func (s *Supervisor) shouldStop(ctx context.Context) bool {
	select {
	case <-s.stopCh:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// sleep blocks for d, returning false if stopCh or ctx fires first.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stopCh:
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *Supervisor) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *Supervisor) markReady(port int) {
	s.mu.Lock()
	s.state = StateReady
	s.port = port
	s.endpoint = endpointURL(s.cfg.Host, port)
	s.lastErr = nil
	first := true
	select {
	case <-s.readyCh:
		first = false
	default:
	}
	if first {
		close(s.readyCh)
	}
	s.mu.Unlock()
	s.logger.Info("whispersup.ready", "endpoint", endpointURL(s.cfg.Host, port))
}

func (s *Supervisor) recordError(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

func (s *Supervisor) incRestart() {
	s.mu.Lock()
	s.restarts++
	s.mu.Unlock()
}

// endpointURL returns the OpenAI-compatible transcription URL for
// host:port. The path matches whisper.cpp's whisper-server defaults.
func endpointURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v1/audio/transcriptions"
}

// nextBackoff doubles d, capped at maxD.
func nextBackoff(d, maxD time.Duration) time.Duration {
	d *= 2
	if d > maxD {
		d = maxD
	}
	return d
}
