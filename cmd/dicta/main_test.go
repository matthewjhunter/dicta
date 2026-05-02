package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/control"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// goleak catches stray daemon-side goroutines from the test fixture
	// (control.Server.Serve) failing to drain when a test exits early.
	goleak.VerifyTestMain(m)
}

// recordingHandler captures every command the CLI sends so tests can
// assert wire-shape correctness without spinning up the real session/
// audio/asr stack.
type recordingHandler struct {
	mu sync.Mutex

	statusCalls   int
	toggleCalls   []string
	commitCalls   []string
	cancelCalls   int
	shutdownCalls int

	statusInfo control.StatusInfo
	cancelErr  error
}

func (h *recordingHandler) Status(_ context.Context) (control.StatusInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statusCalls++
	return h.statusInfo, nil
}

func (h *recordingHandler) ToggleTalk(_ context.Context, mode string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toggleCalls = append(h.toggleCalls, mode)
	return nil
}

func (h *recordingHandler) Commit(_ context.Context, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commitCalls = append(h.commitCalls, text)
	return nil
}

func (h *recordingHandler) Cancel(_ context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelCalls++
	return h.cancelErr
}

func (h *recordingHandler) MicList(_ context.Context) ([]control.MicInfo, error) {
	return nil, control.ErrNotImplemented
}
func (h *recordingHandler) MicSelect(_ context.Context, _ string, _ bool) error {
	return control.ErrNotImplemented
}
func (h *recordingHandler) Subscribe(_ context.Context, _ []string, _ control.EventPush) error {
	return control.ErrNotImplemented
}
func (h *recordingHandler) Shutdown(_ context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shutdownCalls++
	return control.ErrNotImplemented // matches v1 daemon behavior
}

// startServer spins a control.Server bound to a per-test socket and
// returns the path. The server stops when ctx is cancelled.
func startServer(t *testing.T, h control.Handler) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "dicta.sock")

	srv, err := control.Listen(sock, h, func(format string, args ...any) {
		t.Logf("server: "+format, args...)
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-done
	})
	return sock
}

// runCLI invokes the CLI's run() with the test socket path injected
// via --socket and returns exit code + stdout/stderr.
func runCLI(t *testing.T, sock string, args ...string) (int, string, string) {
	t.Helper()
	stdout := captureFile(t, "stdout")
	stderr := captureFile(t, "stderr")

	prepended := append([]string{"--socket", sock}, args...)
	code := run(prepended, stdout, stderr)

	_ = stdout.Close()
	_ = stderr.Close()
	return code, readFile(t, stdout.Name()), readFile(t, stderr.Name())
}

func captureFile(t *testing.T, label string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dicta-cli-"+label+"-*")
	if err != nil {
		t.Fatalf("create %s capture: %v", label, err)
	}
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestStatus_RoundTrip(t *testing.T) {
	h := &recordingHandler{statusInfo: control.StatusInfo{Version: "test", SessionMode: "none"}}
	sock := startServer(t, h)

	code, stdout, stderr := runCLI(t, sock, "status")
	if code != 0 {
		t.Fatalf("exit code: %d (stderr=%q)", code, stderr)
	}
	var resp control.Response
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("decode response: %v (stdout=%q)", err, stdout)
	}
	if !resp.OK {
		t.Errorf("OK: got false; resp=%+v", resp)
	}

	if h.statusCalls != 1 {
		t.Errorf("status calls: got %d want 1", h.statusCalls)
	}
}

func TestToggleTalk_TypeMode(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "toggle_talk", "--mode", "type")
	if code != 0 {
		t.Fatalf("exit code: %d (stderr=%q)", code, stderr)
	}
	if len(h.toggleCalls) != 1 || h.toggleCalls[0] != "type" {
		t.Errorf("toggle calls: got %v want [type]", h.toggleCalls)
	}
}

func TestToggleTalk_ClipMode(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "toggle_talk", "--mode", "clip")
	if code != 0 {
		t.Fatalf("exit code: %d (stderr=%q)", code, stderr)
	}
	if len(h.toggleCalls) != 1 || h.toggleCalls[0] != "clip" {
		t.Errorf("toggle calls: got %v want [clip]", h.toggleCalls)
	}
}

func TestToggleTalk_RejectsMissingMode(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "toggle_talk")
	if code != 2 {
		t.Errorf("exit code: got %d want 2 (usage)", code)
	}
	if len(h.toggleCalls) != 0 {
		t.Errorf("toggle should not have been called; got %v", h.toggleCalls)
	}
	if stderr == "" {
		t.Error("stderr: expected usage error message")
	}
}

func TestToggleTalk_RejectsBogusMode(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, _ := runCLI(t, sock, "toggle_talk", "--mode", "potato")
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
	if len(h.toggleCalls) != 0 {
		t.Errorf("toggle should not have been called; got %v", h.toggleCalls)
	}
}

func TestCommit_PassesTextThrough(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "commit", "--text", "panel-edited text")
	if code != 0 {
		t.Fatalf("exit code: %d (stderr=%q)", code, stderr)
	}
	if len(h.commitCalls) != 1 || h.commitCalls[0] != "panel-edited text" {
		t.Errorf("commit calls: got %v want [panel-edited text]", h.commitCalls)
	}
}

func TestCommit_EmptyTextWarnsButSends(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "commit", "--text", "")
	if code != 0 {
		t.Errorf("exit code: %d (empty text is allowed)", code)
	}
	if len(h.commitCalls) != 1 {
		t.Errorf("commit calls: got %d want 1", len(h.commitCalls))
	}
	// Warning should make it visible that the user committed empty text.
	if stderr == "" {
		t.Error("stderr: expected warning about empty --text")
	}
}

func TestCancel_CallsHandler(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "cancel")
	if code != 0 {
		t.Fatalf("exit code: %d (stderr=%q)", code, stderr)
	}
	if h.cancelCalls != 1 {
		t.Errorf("cancel calls: got %d want 1", h.cancelCalls)
	}
}

func TestCancel_HandlerErrorMapsToExitOne(t *testing.T) {
	h := &recordingHandler{cancelErr: errors.New("not in clip-mode")}
	sock := startServer(t, h)

	code, stdout, _ := runCLI(t, sock, "cancel")
	if code != 1 {
		t.Errorf("exit code: got %d want 1", code)
	}
	var resp control.Response
	_ = json.Unmarshal([]byte(stdout), &resp)
	if resp.OK {
		t.Error("response OK: got true want false")
	}
	if resp.Error == "" {
		t.Error("response Error: want non-empty")
	}
}

func TestShutdown_SurfacesNotImplemented(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, stdout, _ := runCLI(t, sock, "shutdown")
	// v1 daemon returns not_implemented for shutdown — exit code 1
	// and the response code is "not_implemented".
	if code != 1 {
		t.Errorf("exit code: got %d want 1 (v1 returns not_implemented)", code)
	}
	var resp control.Response
	_ = json.Unmarshal([]byte(stdout), &resp)
	if resp.Code != "not_implemented" {
		t.Errorf("response code: got %q want not_implemented", resp.Code)
	}
	if h.shutdownCalls != 1 {
		t.Errorf("shutdown calls: got %d want 1", h.shutdownCalls)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	h := &recordingHandler{}
	sock := startServer(t, h)

	code, _, stderr := runCLI(t, sock, "potato")
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
	if stderr == "" {
		t.Error("stderr: expected usage on unknown subcommand")
	}
}

func TestNoArgs_ShowsUsage(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()

	code := run([]string{}, stdout, stderr)
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
	_, _ = stderr.Seek(0, io.SeekStart)
	out, _ := io.ReadAll(stderr)
	if len(out) == 0 {
		t.Error("stderr: expected usage message")
	}
}

func TestTimeoutHonored(t *testing.T) {
	// Point at a non-existent socket; default 2s timeout would make
	// the test slow, so override to 100ms.
	dir := t.TempDir()
	sock := filepath.Join(dir, "noexist.sock")

	stdout, _ := os.CreateTemp(t.TempDir(), "stdout-*")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr-*")
	defer stdout.Close()
	defer stderr.Close()

	start := time.Now()
	code := run([]string{"--socket", sock, "--timeout", "100ms", "status"}, stdout, stderr)
	elapsed := time.Since(start)

	if code != 1 {
		t.Errorf("exit code: got %d want 1", code)
	}
	if elapsed > time.Second {
		t.Errorf("timeout not honored; elapsed %v", elapsed)
	}
}
