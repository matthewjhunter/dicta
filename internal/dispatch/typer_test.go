package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubTyperConfig points Binary at the test executable in stub mode.
// Each invocation appends one JSON record to the returned logPath.
func stubTyperConfig(t *testing.T) (TyperConfig, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := exe[:strings.LastIndex(exe, "/")]
	logPath := t.TempDir() + "/stub-invocations.jsonl"

	t.Setenv("DICTA_DISPATCH_STUB", "1")
	t.Setenv("DICTA_DISPATCH_STUB_LOG", logPath)

	return TyperConfig{
		Binary:          exe,
		ChunkSize:       200,
		ChunkDelay:      0,
		InvokeTimeout:   2 * time.Second,
		BinaryAllowlist: []string{binDir},
		Logger:          discardLogger(),
	}, logPath
}

func readInvocations(t *testing.T, path string) []stubInvocation {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []stubInvocation
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec stubInvocation
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

func TestTyper_StripsNewlines(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	tp, err := NewSubprocessTyper(cfg)
	if err != nil {
		t.Fatalf("NewSubprocessTyper: %v", err)
	}
	if err := tp.Type(t.Context(), "hello\nworld\rdanger"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d want 1", len(inv))
	}
	// argv = ["type", "--", text]
	if got, want := inv[0].Args[len(inv[0].Args)-1], "hello world danger"; got != want {
		t.Errorf("payload: got %q want %q", got, want)
	}
}

func TestTyper_ChunksLongText(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	cfg.ChunkSize = 10
	cfg.ChunkDelay = 1 * time.Millisecond
	tp, _ := NewSubprocessTyper(cfg)
	// 35 chars; with size=10 and word-boundary preference, expect ~4 chunks.
	text := "the quick brown fox jumps over the dog"
	if err := tp.Type(t.Context(), text); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) < 3 {
		t.Errorf("expected ≥3 chunks for 38-char text at size=10; got %d", len(inv))
	}
	// Reassemble payloads and verify lossless round-trip.
	var got string
	for _, rec := range inv {
		if len(rec.Args) < 3 {
			t.Fatalf("argv too short: %v", rec.Args)
		}
		got += rec.Args[len(rec.Args)-1]
	}
	if got != text {
		t.Errorf("reassembled: got %q want %q", got, text)
	}
}

func TestTyper_PassesSocketEnv(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	cfg.Socket = "/run/test/.ydotool_socket"
	tp, err := NewSubprocessTyper(cfg)
	if err != nil {
		t.Fatalf("NewSubprocessTyper: %v", err)
	}
	if err := tp.Type(t.Context(), "x"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d want 1", len(inv))
	}
	if inv[0].Socket != cfg.Socket {
		t.Errorf("YDOTOOL_SOCKET: got %q want %q", inv[0].Socket, cfg.Socket)
	}
}

func TestTyper_NoSocketLeavesEnvAlone(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	// Caller has already set YDOTOOL_SOCKET in their environment.
	t.Setenv("YDOTOOL_SOCKET", "/preexisting")
	tp, _ := NewSubprocessTyper(cfg)
	if err := tp.Type(t.Context(), "x"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d", len(inv))
	}
	if inv[0].Socket != "/preexisting" {
		t.Errorf("YDOTOOL_SOCKET: got %q want preserved /preexisting", inv[0].Socket)
	}
}

func TestTyper_KeyDelayPropagated(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	cfg.KeyDelay = 12 * time.Millisecond
	tp, _ := NewSubprocessTyper(cfg)
	if err := tp.Type(t.Context(), "hi"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d", len(inv))
	}
	args := inv[0].Args
	// Expect: ["type", "--key-delay", "12", "--", "hi"]
	hasFlag := false
	for i, a := range args {
		if a == "--key-delay" && i+1 < len(args) && args[i+1] == "12" {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		t.Errorf("expected --key-delay 12 in argv; got %v", args)
	}
}

func TestTyper_DashSeparatorPrecedesText(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	tp, _ := NewSubprocessTyper(cfg)
	// Leading '-' would be parsed as a flag without the literal "--".
	if err := tp.Type(t.Context(), "-foo"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	inv := readInvocations(t, logPath)
	if len(inv) != 1 {
		t.Fatalf("invocations: got %d", len(inv))
	}
	args := inv[0].Args
	if len(args) < 2 {
		t.Fatalf("argv too short: %v", args)
	}
	if args[len(args)-2] != "--" {
		t.Errorf("expected '--' immediately before text; got argv %v", args)
	}
}

func TestTyper_EmptyAndWhitespaceNoop(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	tp, _ := NewSubprocessTyper(cfg)
	for _, in := range []string{"", "   ", "\n\n\n", "\t\r\n "} {
		if err := tp.Type(t.Context(), in); err != nil {
			t.Errorf("Type(%q): %v", in, err)
		}
	}
	if got := readInvocations(t, logPath); len(got) != 0 {
		t.Errorf("expected 0 invocations for whitespace-only input; got %d (%+v)", len(got), got)
	}
}

func TestTyper_ContextCancelStopsBetweenChunks(t *testing.T) {
	cfg, logPath := stubTyperConfig(t)
	cfg.ChunkSize = 5
	cfg.ChunkDelay = 200 * time.Millisecond
	tp, _ := NewSubprocessTyper(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel after the first chunk fires.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := tp.Type(ctx, "abcde fghij klmno pqrst uvwxy")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if got := readInvocations(t, logPath); len(got) >= 5 {
		t.Errorf("expected fewer chunks than 5 after cancel; got %d", len(got))
	}
}

func TestTyper_BadExitSurfacedAsError(t *testing.T) {
	cfg, _ := stubTyperConfig(t)
	t.Setenv("DICTA_DISPATCH_STUB_EXIT", "3")
	tp, _ := NewSubprocessTyper(cfg)
	if err := tp.Type(t.Context(), "x"); err == nil {
		t.Error("expected error from non-zero exit")
	}
}

func TestTyper_BinaryNotExistReturnsError(t *testing.T) {
	cfg, _ := stubTyperConfig(t)
	cfg.Binary = "/usr/bin/this-binary-does-not-exist-xyz"
	cfg.BinaryAllowlist = []string{"/usr/bin"}
	tp, err := NewSubprocessTyper(cfg)
	if err != nil {
		t.Fatalf("NewSubprocessTyper: %v", err)
	}
	err = tp.Type(t.Context(), "x")
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestNewSubprocessTyper_RejectsOffAllowlistBinary(t *testing.T) {
	_, err := NewSubprocessTyper(TyperConfig{
		Binary:          "/tmp/ydotool",
		BinaryAllowlist: []string{"/usr/bin"},
	})
	if err == nil {
		t.Error("expected validation error for /tmp binary")
	}
}

func TestNewSubprocessTyper_RejectsRelativeBinary(t *testing.T) {
	_, err := NewSubprocessTyper(TyperConfig{
		Binary: "ydotool",
	})
	if err == nil {
		t.Error("expected validation error for relative path")
	}
}

func TestNewSubprocessTyper_RejectsNonAbsoluteSocket(t *testing.T) {
	cfg, _ := stubTyperConfig(t)
	cfg.Socket = "relative/path"
	_, err := NewSubprocessTyper(cfg)
	if err == nil {
		t.Error("expected validation error for relative socket")
	}
}

func TestChunkString(t *testing.T) {
	cases := []struct {
		s    string
		size int
		want []string
	}{
		{"", 5, []string{""}},
		{"abc", 5, []string{"abc"}},
		{"abcde", 5, []string{"abcde"}},
		{"abcdef", 5, []string{"abcde", "f"}},
		{"hello world", 6, []string{"hello ", "world"}},
		// size=8 with no whitespace in the last quarter → cut at exactly 8.
		{"the quick brown", 8, []string{"the quic", "k brown"}},
		// size=12 lets the algorithm pick the space at index 9 (within the
		// last quarter of the window).
		{"the quick brown fox", 12, []string{"the quick ", "brown fox"}},
	}
	for _, c := range cases {
		got := chunkString(c.s, c.size)
		if len(got) != len(c.want) {
			t.Errorf("chunkString(%q, %d): got %d chunks (%v) want %d (%v)",
				c.s, c.size, len(got), got, len(c.want), c.want)
			continue
		}
		var reassembled string
		for _, ch := range got {
			reassembled += ch
		}
		if reassembled != c.s {
			t.Errorf("chunkString(%q, %d) lost data: %q", c.s, c.size, reassembled)
		}
	}
}
