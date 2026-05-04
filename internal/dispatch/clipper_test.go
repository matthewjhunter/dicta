package dispatch

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func stubClipperConfig(t *testing.T) (ClipperConfig, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := exe[:strings.LastIndex(exe, "/")]
	logPath := t.TempDir() + "/stub-clip-invocations.jsonl"

	t.Setenv("DICTA_DISPATCH_CLIP_STUB", "1")
	t.Setenv("DICTA_DISPATCH_STUB_LOG", logPath)

	return ClipperConfig{
		Binary:          exe,
		InvokeTimeout:   2 * time.Second,
		BinaryAllowlist: []string{binDir},
		Logger:          discardLogger(),
	}, logPath
}

func TestClipper_PipesPayloadToStdin(t *testing.T) {
	cfg, logPath := stubClipperConfig(t)
	c, err := NewSubprocessClipper(cfg)
	if err != nil {
		t.Fatalf("NewSubprocessClipper: %v", err)
	}
	if err := c.Clip(t.Context(), "hello panel"); err != nil {
		t.Fatalf("Clip: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d want 1", len(inv))
	}
	if inv[0].Stdin != "hello panel" {
		t.Errorf("stdin: got %q want %q", inv[0].Stdin, "hello panel")
	}
}

func TestClipper_PreservesNewlines(t *testing.T) {
	// Unlike Typer, Clipper does not strip newlines: clip-mode allows
	// the user to insert literal newlines via Shift+Enter and they must
	// land on the clipboard verbatim.
	cfg, logPath := stubClipperConfig(t)
	c, _ := NewSubprocessClipper(cfg)
	payload := "line one\nline two\rline three"
	if err := c.Clip(t.Context(), payload); err != nil {
		t.Fatalf("Clip: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d", len(inv))
	}
	if inv[0].Stdin != payload {
		t.Errorf("stdin: got %q want %q", inv[0].Stdin, payload)
	}
}

func TestClipper_PassesExtraArgs(t *testing.T) {
	cfg, logPath := stubClipperConfig(t)
	cfg.ExtraArgs = []string{"--type", "text/plain"}
	c, _ := NewSubprocessClipper(cfg)
	if err := c.Clip(t.Context(), "x"); err != nil {
		t.Fatalf("Clip: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d", len(inv))
	}
	args := inv[0].Args
	if len(args) < 2 || args[0] != "--type" || args[1] != "text/plain" {
		t.Errorf("argv: got %v want [--type, text/plain]", args)
	}
}

func TestClipper_EmptyPayloadCommitted(t *testing.T) {
	// An empty Clip call still invokes wl-copy — the orchestrator
	// might want to clear the clipboard or replace its contents with
	// nothing. The dispatch layer should not second-guess that.
	cfg, logPath := stubClipperConfig(t)
	c, _ := NewSubprocessClipper(cfg)
	if err := c.Clip(t.Context(), ""); err != nil {
		t.Fatalf("Clip: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Errorf("expected 1 invocation for empty payload; got %d", len(inv))
	}
}

func TestClipper_BadExitSurfacedAsError(t *testing.T) {
	cfg, _ := stubClipperConfig(t)
	t.Setenv("DICTA_DISPATCH_STUB_EXIT", "5")
	c, _ := NewSubprocessClipper(cfg)
	if err := c.Clip(t.Context(), "x"); err == nil {
		t.Error("expected error from non-zero exit")
	}
}

func TestClipper_ContextCancelSurfaced(t *testing.T) {
	cfg, _ := stubClipperConfig(t)
	cfg.InvokeTimeout = time.Hour // leave the kill to ctx
	// Stub drains stdin in <1ms, so without a block hook the cancel
	// goroutine races and finds the process already gone. 500ms is plenty
	// for a 10ms-delayed cancel even on a loaded CI runner; if cancel
	// silently fails to fire, the test bounds at 500ms instead of 1h.
	t.Setenv("DICTA_DISPATCH_STUB_BLOCK_MS", "500")
	c, _ := NewSubprocessClipper(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := c.Clip(ctx, strings.Repeat("x", 1<<20))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestNewSubprocessClipper_RejectsOffAllowlistBinary(t *testing.T) {
	_, err := NewSubprocessClipper(ClipperConfig{
		Binary:          "/tmp/wl-copy",
		BinaryAllowlist: []string{"/usr/bin"},
	})
	if err == nil {
		t.Error("expected validation error for /tmp binary")
	}
}

func TestNewSubprocessClipper_RejectsRelativeBinary(t *testing.T) {
	_, err := NewSubprocessClipper(ClipperConfig{
		Binary: "wl-copy",
	})
	if err == nil {
		t.Error("expected validation error for relative path")
	}
}

func TestClipper_BinaryNotExistReturnsError(t *testing.T) {
	cfg, _ := stubClipperConfig(t)
	cfg.Binary = "/usr/bin/this-binary-does-not-exist-xyz"
	cfg.BinaryAllowlist = []string{"/usr/bin"}
	c, _ := NewSubprocessClipper(cfg)
	if err := c.Clip(t.Context(), "x"); err == nil {
		t.Error("expected error for missing binary")
	}
}
