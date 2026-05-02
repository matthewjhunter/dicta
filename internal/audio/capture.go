package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Capture is the audio-source interface owned by internal/audio. The
// orchestrator calls Start to receive a frame channel, then Stop to tear
// down the subprocess.
type Capture interface {
	Start(ctx context.Context) (<-chan Frame, error)
	Stop() error
	Backend() string // "pipewire" | "pulse"
}

// Backend selects which subprocess captures audio.
type CaptureBackend string

const (
	BackendAuto     CaptureBackend = "auto"
	BackendPipeWire CaptureBackend = "pipewire"
	BackendPulse    CaptureBackend = "pulse"
)

// CaptureConfig parameterizes the subprocess capture.
//
// Backend "auto" picks pipewire when pw-record is on PATH and falls back
// to pulse (parec) otherwise. Device is the PipeWire node name (or the
// pulse source name); empty means "system default at session start" per
// §5.1's source-of-truth chain. The orchestrator is responsible for
// resolving the chain (state file → user config → default) before
// calling.
type CaptureConfig struct {
	Backend  CaptureBackend
	Device   string
	BufferMS int // optional per-process latency hint; 0 = let server choose
}

// SubprocessCapture spawns pw-record (preferred) or parec (fallback) and
// reads raw S16LE 16 kHz mono PCM from its stdout, emitting one Frame per
// FrameBytes-aligned chunk. Subprocess argv is built from typed fields —
// no shell, no string interpolation that could carry a user payload (§8).
type SubprocessCapture struct {
	cfg     CaptureConfig
	backend string

	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	frames chan Frame
}

// NewSubprocessCapture returns an unstarted SubprocessCapture configured
// from cfg. Backend selection happens at Start time so we can probe the
// environment freshly per session.
func NewSubprocessCapture(cfg CaptureConfig) *SubprocessCapture {
	return &SubprocessCapture{cfg: cfg}
}

// Backend returns the subprocess backend selected by the most recent
// Start (or "" if not started).
func (c *SubprocessCapture) Backend() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backend
}

// Start spawns the capture subprocess and returns a frame channel. Stop
// or ctx cancellation halts the subprocess and closes the channel.
func (c *SubprocessCapture) Start(ctx context.Context) (<-chan Frame, error) {
	c.mu.Lock()
	if c.cmd != nil {
		c.mu.Unlock()
		return nil, errors.New("capture already started")
	}

	backend, name, args, err := c.resolveCommand()
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}

	subCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(subCtx, name, args...)
	cmd.Stderr = nil // discard subprocess stderr for now; phase 11 (audit) wires this up
	// Put the subprocess in its own process group so we can kill the
	// whole group on cancel — important for shell-pipeline-style
	// stubs in tests, and a defense-in-depth for any future wrapper
	// scripts shipped around pw-record/parec.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID targets the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		c.mu.Unlock()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		c.mu.Unlock()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	frames := make(chan Frame, 8)
	done := make(chan struct{})

	c.cmd = cmd
	c.cancel = cancel
	c.done = done
	c.frames = frames
	c.backend = backend
	c.mu.Unlock()

	go c.pump(stdout, frames, done)

	return frames, nil
}

// Stop terminates the subprocess and waits for the read loop to exit.
// Safe to call multiple times.
func (c *SubprocessCapture) Stop() error {
	c.mu.Lock()
	if c.cmd == nil {
		c.mu.Unlock()
		return nil
	}
	cancel := c.cancel
	done := c.done
	cmd := c.cmd
	c.cmd = nil
	c.cancel = nil
	c.done = nil
	c.frames = nil
	c.mu.Unlock()

	cancel()
	if done != nil {
		<-done
	}
	// CommandContext kills the process when the context is cancelled; Wait
	// returns the resulting "killed" error which is expected.
	_ = cmd.Wait()
	return nil
}

// pump reads FrameBytes-aligned chunks from r and emits Frames until r
// returns an error or the subprocess exits.
func (c *SubprocessCapture) pump(r io.ReadCloser, out chan<- Frame, done chan<- struct{}) {
	defer close(out)
	defer close(done)
	defer r.Close()

	buf := make([]byte, FrameBytes)
	for {
		_, err := io.ReadFull(r, buf)
		if err != nil {
			return
		}
		// Copy into a fresh slice so downstream consumers and the ring
		// buffer don't alias the read buffer.
		pcm := make([]byte, FrameBytes)
		copy(pcm, buf)
		select {
		case out <- Frame{PCM: pcm, Timestamp: time.Now()}:
		default:
			// Drop on full channel rather than block the audio thread.
			// The ring buffer + tests catch starvation; in practice this
			// means a downstream consumer is wedged.
		}
	}
}

// resolveCommand returns the (backend label, exe, argv) for the configured
// or auto-selected backend. Argv is built only from typed values.
func (c *SubprocessCapture) resolveCommand() (backend, exe string, args []string, err error) {
	pick := c.cfg.Backend
	if pick == "" {
		pick = BackendAuto
	}
	if pick == BackendAuto {
		if _, err := exec.LookPath("pw-record"); err == nil {
			pick = BackendPipeWire
		} else {
			pick = BackendPulse
		}
	}
	switch pick {
	case BackendPipeWire:
		exe = "pw-record"
		args = pipewireArgs(c.cfg)
		backend = "pipewire"
	case BackendPulse:
		exe = "parec"
		args = pulseArgs(c.cfg)
		backend = "pulse"
	default:
		return "", "", nil, fmt.Errorf("unknown backend %q", pick)
	}
	if _, err := exec.LookPath(exe); err != nil {
		return "", "", nil, fmt.Errorf("%s not on PATH: %w", exe, err)
	}
	return backend, exe, args, nil
}

// pipewireArgs builds argv for pw-record. Format flags align with D15.
func pipewireArgs(cfg CaptureConfig) []string {
	args := []string{
		"--rate=" + strconv.Itoa(SampleRateHz),
		"--channels=" + strconv.Itoa(Channels),
		"--format=s16",
	}
	if cfg.Device != "" {
		args = append(args, "--target="+cfg.Device)
	}
	// pw-record writes WAV by default unless we point it at "-" with raw
	// flags. The empty file-name positional makes it write to stdout, and
	// --raw skips the WAV header so consumers see plain S16LE.
	args = append(args, "--raw", "-")
	return args
}

// pulseArgs builds argv for parec.
func pulseArgs(cfg CaptureConfig) []string {
	args := []string{
		"--rate=" + strconv.Itoa(SampleRateHz),
		"--channels=" + strconv.Itoa(Channels),
		"--format=s16le",
		"--raw",
	}
	if cfg.Device != "" {
		args = append(args, "--device="+cfg.Device)
	}
	if cfg.BufferMS > 0 {
		// parec takes latency-msec; pw-record has no equivalent v1 knob.
		args = append(args, "--latency-msec="+strconv.Itoa(cfg.BufferMS))
	}
	return args
}
