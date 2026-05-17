package whispersup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubConfig builds a Config that points Binary at the test executable
// running in stub mode. Allowlists are set to the binary's actual
// directory so Validate accepts it.
func stubConfig(t *testing.T, extraEnv ...string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := exe[:strings.LastIndex(exe, "/")]
	modelDir := t.TempDir()
	model := modelDir + "/ggml-stub.bin"
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append([]string{"DICTA_WHISPERSUP_STUB=1"}, extraEnv...)
	return Config{
		Binary:                exe,
		ModelPath:             model,
		Host:                  "127.0.0.1",
		Port:                  0,
		Threads:               2,
		Env:                   env,
		StartupTimeout:        3 * time.Second,
		RestartBackoffInitial: 100 * time.Millisecond,
		RestartBackoffMax:     500 * time.Millisecond,
		BinaryAllowlist:       []string{binDir},
		ModelPathAllowlist:    []string{modelDir},
	}
}

func TestSupervisor_StartsAndReadiness(t *testing.T) {
	cfg := stubConfig(t)
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := sup.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v (state=%s lastErr=%v)", err, sup.State(), sup.LastError())
	}
	if sup.State() != StateReady {
		t.Errorf("State: got %s want ready", sup.State())
	}
	endpoint := sup.Endpoint()
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Errorf("Endpoint: got %q", endpoint)
	}
	if !strings.HasSuffix(endpoint, "/v1/audio/transcriptions") {
		t.Errorf("Endpoint: got %q", endpoint)
	}

	// Verify the stub server actually responds on the endpoint.
	resp, err := http.Post(endpoint, "application/octet-stream", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
}

func TestSupervisor_FixedPort(t *testing.T) {
	// Pick a port ourselves to verify Port != 0 path.
	port, err := pickFreePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	cfg := stubConfig(t)
	cfg.Port = port
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := sup.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v (state=%s lastErr=%v)", err, sup.State(), sup.LastError())
	}
	if !strings.Contains(sup.Endpoint(), strconv.Itoa(port)) {
		t.Errorf("Endpoint should include configured port %d: %q", port, sup.Endpoint())
	}
}

func TestSupervisor_RestartOnCrash(t *testing.T) {
	cfg := stubConfig(t, "DICTA_WHISPERSUP_STUB_CRASH_AFTER_MS=500")
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := sup.WaitReady(ctx); err != nil {
		t.Fatalf("first WaitReady: %v", err)
	}

	// Wait for the crash and at least one restart attempt.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sup.Restarts() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sup.Restarts() < 1 {
		t.Errorf("expected at least 1 restart; got %d (state=%s)", sup.Restarts(), sup.State())
	}
}

func TestSupervisor_ProbeFailureBacksOff(t *testing.T) {
	cfg := stubConfig(t, "DICTA_WHISPERSUP_STUB_REFUSE_TO_BIND=1")
	cfg.StartupTimeout = 300 * time.Millisecond
	cfg.RestartBackoffMax = 100 * time.Millisecond
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	probeCalls := atomic.Int32{}
	sup.SetProbe(func(ctx context.Context, host string, port int) error {
		probeCalls.Add(1)
		return TCPConnectProbe(ctx, host, port)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	// Should never become ready.
	err = sup.WaitReady(ctx)
	if err == nil {
		t.Errorf("WaitReady should not have succeeded; state=%s", sup.State())
	}
	if probeCalls.Load() < 2 {
		t.Errorf("expected ≥2 probe attempts (initial + retry); got %d", probeCalls.Load())
	}
	if sup.LastError() == nil {
		t.Error("expected LastError to be set after probe failure")
	}
}

func TestSupervisor_StopBeforeReady(t *testing.T) {
	cfg := stubConfig(t, "DICTA_WHISPERSUP_STUB_REFUSE_TO_BIND=1")
	cfg.StartupTimeout = 5 * time.Second
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan struct{})
	go func() {
		sup.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not complete within 10 s")
	}

	if sup.State() != StateStopped {
		t.Errorf("State: got %s want stopped", sup.State())
	}
}

func TestSupervisor_StartTwice(t *testing.T) {
	cfg := stubConfig(t)
	sup, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err == nil {
		t.Error("second Start should error")
	}
}

func TestSupervisor_New_RejectsBadConfig(t *testing.T) {
	_, err := New(Config{Binary: "/tmp/x", ModelPath: "/var/lib/dicta/m.bin"}, discardLogger())
	if err == nil {
		t.Error("expected validation error for /tmp binary")
	}
}

func TestSupervisor_New_RequiresLogger(t *testing.T) {
	cfg := stubConfig(t)
	_, err := New(cfg, nil)
	if err == nil {
		t.Error("expected error for nil logger")
	}
}

func TestPickFreePort(t *testing.T) {
	p, err := pickFreePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if p == 0 {
		t.Errorf("expected non-zero port")
	}
	// Verify port really is free immediately after.
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Errorf("port %d not actually free: %v", p, err)
	} else {
		l.Close()
	}
}

func TestTCPConnectProbe_FailsFast(t *testing.T) {
	// Port 1 is reserved; this should fail.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := TCPConnectProbe(ctx, "127.0.0.1", 1)
	if err == nil {
		t.Error("expected probe failure on port 1")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// If it failed for a different reason that's fine, but the
		// context-driven path is what we want covered.
		t.Logf("non-deadline error (acceptable): %v", err)
	}
}

func TestState_String(t *testing.T) {
	cases := map[State]string{
		StateIdle: "idle", StateStarting: "starting", StateReady: "ready",
		StateCrashed: "crashed", StateStopping: "stopping", StateStopped: "stopped",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d: got %q want %q", s, got, want)
		}
	}
	if got := State(99).String(); got != "unknown" {
		t.Errorf("unknown: got %q", got)
	}
}
