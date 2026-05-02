package audio

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPump_EmitsAlignedFrames(t *testing.T) {
	// Build 3 frames of distinguishable PCM.
	src := make([]byte, 3*FrameBytes)
	for i := range src {
		src[i] = byte(i)
	}

	c := &SubprocessCapture{}
	frames := make(chan Frame, 4)
	done := make(chan struct{})
	r := io.NopCloser(bytes.NewReader(src))

	go c.pump(r, frames, done)
	<-done // pump exits when reader is exhausted

	var got []Frame
	for f := range frames {
		got = append(got, f)
	}
	if len(got) != 3 {
		t.Fatalf("frame count: got %d want 3", len(got))
	}
	for i, f := range got {
		if len(f.PCM) != FrameBytes {
			t.Errorf("frame %d size: got %d want %d", i, len(f.PCM), FrameBytes)
		}
		want := src[i*FrameBytes : (i+1)*FrameBytes]
		if !bytes.Equal(f.PCM, want) {
			t.Errorf("frame %d content mismatch", i)
		}
	}
}

func TestPump_FramesAreIndependentCopies(t *testing.T) {
	// Two frames; if pump aliased its read buffer the second frame would
	// overwrite the first.
	src := append(bytes.Repeat([]byte{0x11}, FrameBytes), bytes.Repeat([]byte{0x22}, FrameBytes)...)
	c := &SubprocessCapture{}
	frames := make(chan Frame, 4)
	done := make(chan struct{})
	go c.pump(io.NopCloser(bytes.NewReader(src)), frames, done)
	<-done

	first := <-frames
	second := <-frames
	if first.PCM[0] != 0x11 {
		t.Errorf("first frame: got %x want 0x11", first.PCM[0])
	}
	if second.PCM[0] != 0x22 {
		t.Errorf("second frame: got %x want 0x22", second.PCM[0])
	}
}

func TestPump_PartialFrameAtEOFIsDropped(t *testing.T) {
	// 1.5 frames of input — second frame is incomplete and must be dropped
	// (io.ReadFull returns ErrUnexpectedEOF on partial reads).
	src := make([]byte, FrameBytes+FrameBytes/2)
	c := &SubprocessCapture{}
	frames := make(chan Frame, 4)
	done := make(chan struct{})
	go c.pump(io.NopCloser(bytes.NewReader(src)), frames, done)
	<-done

	count := 0
	for range frames {
		count++
	}
	if count != 1 {
		t.Errorf("frame count: got %d want 1 (partial frame dropped)", count)
	}
}

func TestPipewireArgs_Shape(t *testing.T) {
	args := pipewireArgs(CaptureConfig{Device: "alsa_input.usb"})
	must := []string{"--rate=16000", "--channels=1", "--format=s16", "--raw", "-"}
	for _, m := range must {
		if !slices.Contains(args, m) {
			t.Errorf("missing arg %q in %v", m, args)
		}
	}
	if !slices.Contains(args, "--target=alsa_input.usb") {
		t.Errorf("device not propagated as --target=: %v", args)
	}

	// Empty device → no --target.
	args = pipewireArgs(CaptureConfig{})
	for _, a := range args {
		if strings.HasPrefix(a, "--target=") {
			t.Errorf("expected no --target with empty device, got %v", args)
		}
	}
}

func TestPulseArgs_Shape(t *testing.T) {
	args := pulseArgs(CaptureConfig{Device: "alsa_input.usb", BufferMS: 50})
	must := []string{"--rate=16000", "--channels=1", "--format=s16le", "--raw",
		"--device=alsa_input.usb", "--latency-msec=50"}
	for _, m := range must {
		if !slices.Contains(args, m) {
			t.Errorf("missing arg %q in %v", m, args)
		}
	}

	// BufferMS 0 → no latency arg.
	args = pulseArgs(CaptureConfig{})
	for _, a := range args {
		if strings.HasPrefix(a, "--latency-msec=") {
			t.Errorf("expected no --latency-msec with BufferMS=0, got %v", args)
		}
	}
}

func TestResolveCommand_AutoPicksPulseWhenPipewireMissing(t *testing.T) {
	// Stub a fake `parec` on PATH but no pw-record.
	dir := t.TempDir()
	stub := filepath.Join(dir, "parec")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	c := NewSubprocessCapture(CaptureConfig{Backend: BackendAuto})
	backend, exe, _, err := c.resolveCommand()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if backend != "pulse" {
		t.Errorf("backend: got %q want pulse", backend)
	}
	if exe != "parec" {
		t.Errorf("exe: got %q want parec", exe)
	}
}

func TestResolveCommand_AutoPicksPipewireWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"pw-record", "parec"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	c := NewSubprocessCapture(CaptureConfig{Backend: BackendAuto})
	backend, exe, _, err := c.resolveCommand()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if backend != "pipewire" {
		t.Errorf("backend: got %q want pipewire", backend)
	}
	if exe != "pw-record" {
		t.Errorf("exe: got %q want pw-record", exe)
	}
}

func TestResolveCommand_MissingBinaryErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := NewSubprocessCapture(CaptureConfig{Backend: BackendPipeWire})
	if _, _, _, err := c.resolveCommand(); err == nil {
		t.Fatal("expected error when pw-record missing")
	}
}

func TestStartStop_RoundTrip(t *testing.T) {
	// End-to-end: spawn a subprocess that writes 2 frames of zeros then exits.
	// We use `head` on /dev/zero to avoid depending on pw-record/parec being
	// installed.
	dir := t.TempDir()
	stub := filepath.Join(dir, "pw-record")
	script := "#!/bin/sh\nhead -c " + itoa(2*FrameBytes) + " /dev/zero\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	c := NewSubprocessCapture(CaptureConfig{Backend: BackendPipeWire})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frames, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := 0
loop:
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				break loop
			}
			got++
		case <-ctx.Done():
			t.Fatal("timeout waiting for frames")
		}
	}

	if got != 2 {
		t.Errorf("frame count: got %d want 2", got)
	}
	if c.Backend() != "pipewire" {
		t.Errorf("Backend(): got %q want pipewire", c.Backend())
	}
	if err := c.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestStart_Twice(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	c := NewSubprocessCapture(CaptureConfig{Backend: BackendPipeWire})
	ctx := t.Context()

	if _, err := c.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := c.Start(ctx); err == nil {
		t.Fatal("second Start should error")
	}
	_ = c.Stop()
}

// itoa is a CGO-free strconv shim to keep the test imports tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
