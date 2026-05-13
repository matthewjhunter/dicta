package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewPathOnAllowlist(t *testing.T) {
	allow := []string{"/usr/bin", "/usr/local/bin", "/opt", "/home/u/.local/bin"}
	cases := []struct {
		path    string
		wantErr bool
	}{
		{"/usr/bin/dicta-preview", false},
		{"/usr/local/bin/dicta-preview", false},
		{"/opt/dicta/preview", false},
		{"/home/u/.local/bin/dicta-preview", false},
		{"/home/u/.local/bin", false},
		{"/home/u/.localbin/dicta-preview", true}, // prefix-without-separator must NOT match
		{"/etc/dicta-preview", true},
		{"relative/path", true},
		{"", true},
	}
	for _, tc := range cases {
		err := previewPathOnAllowlist(tc.path, allow)
		if (err != nil) != tc.wantErr {
			t.Errorf("previewPathOnAllowlist(%q) err=%v, wantErr=%v", tc.path, err, tc.wantErr)
		}
	}
}

func TestDefaultPreviewAllowlist_IncludesUserLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := defaultPreviewAllowlist()

	wantUserBin := filepath.Join(home, ".local", "bin")
	var hasUserBin, hasUsrBin, hasUsrLocal, hasOpt bool
	for _, p := range got {
		switch p {
		case wantUserBin:
			hasUserBin = true
		case "/usr/bin":
			hasUsrBin = true
		case "/usr/local/bin":
			hasUsrLocal = true
		case "/opt":
			hasOpt = true
		}
	}
	if !hasUserBin {
		t.Errorf("allowlist missing %q; got %v", wantUserBin, got)
	}
	if !hasUsrBin || !hasUsrLocal || !hasOpt {
		t.Errorf("allowlist missing system prefixes; got %v", got)
	}
}

func TestNewPreviewProc_AcceptsUserLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(binDir, "dicta-preview")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := newPreviewProc(previewConfig{
		Binary: binPath,
		Socket: "/run/user/1000/dicta.sock",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("expected user-local bin to be allowed: %v", err)
	}
}

func TestNewPreviewProc_RejectsOffAllowlistBinary(t *testing.T) {
	t.Setenv("HOME", "/home/u")

	_, err := newPreviewProc(previewConfig{
		Binary: "/etc/dicta-preview",
		Socket: "/run/user/1000/dicta.sock",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err == nil {
		t.Fatal("expected off-allowlist binary to be rejected")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should mention allowlist; got %v", err)
	}
}
