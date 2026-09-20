package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listenNotify binds a unixgram socket at addr and returns a function
// that reads one datagram from it, mirroring what systemd does with
// $NOTIFY_SOCKET.
func listenNotify(t *testing.T, addr string) func() string {
	t.Helper()
	ua, err := net.ResolveUnixAddr("unixgram", addr)
	if err != nil {
		t.Fatalf("ResolveUnixAddr(%q): %v", addr, err)
	}
	conn, err := net.ListenUnixgram("unixgram", ua)
	if err != nil {
		t.Fatalf("ListenUnixgram(%q): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return func() string {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read notification: %v", err)
		}
		return string(buf[:n])
	}
}

func TestSDNotifyToPathSocket(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "notify.sock")
	read := listenNotify(t, addr)

	if err := sdNotifyTo(addr, "READY=1"); err != nil {
		t.Fatalf("sdNotifyTo: %v", err)
	}
	if got := read(); got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}

func TestSDNotifyToAbstractSocket(t *testing.T) {
	// systemd uses an abstract-namespace socket in the common case;
	// the leading "@" is the textual form of the leading NUL byte.
	addr := "@dicta-test-notify"
	read := listenNotify(t, addr)

	if err := sdNotifyTo(addr, "READY=1"); err != nil {
		t.Fatalf("sdNotifyTo: %v", err)
	}
	if got := read(); got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}

func TestSDNotifyToEmptyAddrIsNoOp(t *testing.T) {
	// Not running under systemd: notification is skipped, not an error.
	if err := sdNotifyTo("", "READY=1"); err != nil {
		t.Errorf("sdNotifyTo with empty addr: got %v, want nil", err)
	}
}

func TestSDNotifyToRejectsMalformedAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{name: "relative path", addr: "notify.sock"},
		{name: "bare name", addr: "systemd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sdNotifyTo(tt.addr, "READY=1")
			if err == nil {
				t.Fatalf("sdNotifyTo(%q): got nil, want error", tt.addr)
			}
			if !strings.Contains(err.Error(), tt.addr) {
				t.Errorf("error %q does not name the bad address %q", err, tt.addr)
			}
		})
	}
}

func TestSDNotifyToRejectsEmptyState(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "notify.sock")
	listenNotify(t, addr)

	if err := sdNotifyTo(addr, ""); err == nil {
		t.Error("sdNotifyTo with empty state: got nil, want error")
	}
}

func TestNotifyReadyUsesEnvironment(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "notify.sock")
	read := listenNotify(t, addr)
	t.Setenv(notifySocketEnv, addr)

	if err := notifyReady(); err != nil {
		t.Fatalf("notifyReady: %v", err)
	}
	if got := read(); got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}

func TestNotifyReadyWithoutEnvironmentIsNoOp(t *testing.T) {
	// An empty value is indistinguishable from unset via os.Getenv, and
	// t.Setenv restores the original for the rest of the package's tests.
	t.Setenv(notifySocketEnv, "")
	if err := notifyReady(); err != nil {
		t.Errorf("notifyReady outside systemd: got %v, want nil", err)
	}
}
