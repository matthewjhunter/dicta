package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
)

// TestMain doubles as both ydotool and wl-copy test stubs. The mode is
// chosen by env var:
//
//   - DICTA_DISPATCH_STUB=1: Typer stub. Records argv + YDOTOOL_SOCKET.
//   - DICTA_DISPATCH_CLIP_STUB=1: Clipper stub. Reads stdin and records
//     argv + the captured payload.
//
// Each stub appends one JSON record to the file named by
// DICTA_DISPATCH_STUB_LOG so tests can assert the invocation shape.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("DICTA_DISPATCH_STUB") != "":
		if err := runStubTyper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv("DICTA_DISPATCH_CLIP_STUB") != "":
		if err := runStubClipper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

type stubInvocation struct {
	Args   []string `json:"args"`
	Socket string   `json:"socket"`
	Stdin  string   `json:"stdin,omitempty"`
	PID    int      `json:"pid"`
}

func runStubTyper() error {
	if exitStr := os.Getenv("DICTA_DISPATCH_STUB_EXIT"); exitStr != "" {
		if code, err := strconv.Atoi(exitStr); err == nil && code != 0 {
			os.Exit(code)
		}
	}

	logPath := os.Getenv("DICTA_DISPATCH_STUB_LOG")
	if logPath == "" {
		return nil
	}
	rec := stubInvocation{
		Args:   os.Args[1:],
		Socket: os.Getenv("YDOTOOL_SOCKET"),
		PID:    os.Getpid(),
	}
	return appendInvocation(logPath, rec)
}

func runStubClipper() error {
	if exitStr := os.Getenv("DICTA_DISPATCH_STUB_EXIT"); exitStr != "" {
		if code, err := strconv.Atoi(exitStr); err == nil && code != 0 {
			os.Exit(code)
		}
	}

	stdinBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}

	logPath := os.Getenv("DICTA_DISPATCH_STUB_LOG")
	if logPath == "" {
		return nil
	}
	rec := stubInvocation{
		Args:  os.Args[1:],
		Stdin: string(stdinBytes),
		PID:   os.Getpid(),
	}
	return appendInvocation(logPath, rec)
}

func appendInvocation(path string, rec stubInvocation) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}
