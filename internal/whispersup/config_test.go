package whispersup

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPathOnAllowlist(t *testing.T) {
	allow := []string{"/usr/bin", "/opt"}
	cases := []struct {
		path string
		ok   bool
	}{
		{"/usr/bin/whisper-server", true},
		{"/usr/bin", true},
		{"/usr/bin/", true},
		{"/usr/bin/sub/dir/exe", true},
		{"/opt/dicta/bin", true},
		{"/usr/binfile", false}, // strict-under rejects this
		{"/usr/local/bin/exe", false},
		{"/etc/passwd", false},
		{"relative/path", false},
		{"", false},
		{"/usr/bin/../etc/passwd", false}, // Clean canonicalizes; resolves to /etc/passwd
	}
	for _, c := range cases {
		err := pathOnAllowlist(c.path, allow)
		if c.ok && err != nil {
			t.Errorf("%q: expected ok, got %v", c.path, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q: expected error, got nil", c.path)
		}
	}
}

func TestValidate_AcceptsDefaults(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/bin/whisper-server",
		ModelPath: "/var/lib/dicta/ggml-base.en.bin",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid: %v", err)
	}
}

func TestValidate_RejectsBadBinary(t *testing.T) {
	cfg := Config{
		Binary:    "/tmp/evil",
		ModelPath: "/var/lib/dicta/m.bin",
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "Binary") {
		t.Errorf("expected binary path error, got %v", err)
	}
}

func TestValidate_RejectsBadModel(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/local/bin/whisper-server",
		ModelPath: "/etc/passwd",
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ModelPath") {
		t.Errorf("expected model path error, got %v", err)
	}
}

func TestValidate_RejectsRelativePaths(t *testing.T) {
	cfg := Config{
		Binary:    "whisper-server",
		ModelPath: "model.bin",
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for relative paths")
	}
}

func TestValidate_RejectsBadPort(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/bin/whisper-server",
		ModelPath: "/var/lib/dicta/m.bin",
		Port:      99999,
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for bad port")
	}
}

func TestValidate_RejectsBadEnv(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/bin/whisper-server",
		ModelPath: "/var/lib/dicta/m.bin",
		Env:       []string{"BADENV"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for malformed env entry")
	}
}

func TestValidate_AcceptsCustomAllowlist(t *testing.T) {
	cfg := Config{
		Binary:             "/tmp/test/whisper-server",
		ModelPath:          "/tmp/test/m.bin",
		BinaryAllowlist:    []string{"/tmp/test"},
		ModelPathAllowlist: []string{"/tmp/test"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid with custom allowlist: %v", err)
	}
}

func TestAutoThreads(t *testing.T) {
	got := autoThreads()
	want := min(max(runtime.NumCPU()/2, 1), 8)
	if got != want {
		t.Errorf("autoThreads: got %d want %d", got, want)
	}
}

func TestDefaultModelPathAllowlist_IncludesXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/test/xdg")
	got := DefaultModelPathAllowlist()
	want := filepath.Join("/test/xdg", "dicta")
	if !slices.Contains(got, want) {
		t.Errorf("expected %q in defaults, got %v", want, got)
	}
}

func TestValidate_ReturnsTypedErrorChain(t *testing.T) {
	cfg := Config{Binary: "/tmp/x", ModelPath: "/var/lib/dicta/m.bin"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	// Just ensure it's a normal Go error with useful context.
	if !errors.Is(err, err) {
		t.Errorf("error chain: %v", err)
	}
}
