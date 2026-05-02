package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// TestMain doubles as the ydotool test stub. The Typer's Binary field is
// pointed at os.Executable(); stub mode is gated by DICTA_DISPATCH_STUB.
// Each invocation appends a JSON record (argv + env subset) to the file
// named by DICTA_DISPATCH_STUB_LOG so tests can assert what ydotool was
// called with.
func TestMain(m *testing.M) {
	if os.Getenv("DICTA_DISPATCH_STUB") == "" {
		os.Exit(m.Run())
	}
	if err := runStubTyper(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type stubInvocation struct {
	Args   []string `json:"args"`
	Socket string   `json:"socket"`
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
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(rec)
}
