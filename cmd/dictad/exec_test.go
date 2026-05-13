package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureExecutable_AcceptsExecutableRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureExecutable("test-binary", path); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestEnsureExecutable_RejectsMissingPath(t *testing.T) {
	err := ensureExecutable("test-binary", "/nonexistent/binary-xyz")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say 'not found'; got %v", err)
	}
	if !strings.Contains(err.Error(), "test-binary") {
		t.Errorf("error should include label; got %v", err)
	}
}

func TestEnsureExecutable_RejectsEmptyPath(t *testing.T) {
	err := ensureExecutable("test-binary", "")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty path; got %v", err)
	}
}

func TestEnsureExecutable_RejectsNonExecutableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-x")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ensureExecutable("test-binary", path)
	if err == nil {
		t.Fatal("expected error for non-executable file")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error should say 'not executable'; got %v", err)
	}
}

func TestEnsureExecutable_RejectsDirectory(t *testing.T) {
	err := ensureExecutable("test-binary", t.TempDir())
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error should say 'not a regular file'; got %v", err)
	}
}

func TestEnsureExecutable_AcceptsSymlinkToExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureExecutable("test-binary", link); err != nil {
		t.Errorf("expected ok for symlink to executable, got %v", err)
	}
}
