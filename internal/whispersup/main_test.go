package whispersup

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMain doubles as the test stub binary. The supervisor's Binary
// field is pointed at os.Executable(), and the stub mode is signalled
// via the env var DICTA_WHISPERSUP_STUB. When set, the test binary
// parses the same -m / --host / --port flags whisper-server would
// accept and runs a tiny HTTP listener so the readiness probe and
// crash-restart paths can be exercised hermetically.
func TestMain(m *testing.M) {
	if os.Getenv("DICTA_WHISPERSUP_STUB") == "" {
		os.Exit(m.Run())
	}
	if err := runStubServer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runStubServer() error {
	fs := flag.NewFlagSet("stub-whisper-server", flag.ContinueOnError)
	model := fs.String("m", "", "model path (ignored)")
	host := fs.String("host", "127.0.0.1", "")
	port := fs.Int("port", 0, "")
	threads := fs.Int("t", 1, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	_ = model
	_ = threads

	// Behavioral flags from env so we don't have to teach the supervisor
	// to pass them through.
	if d := os.Getenv("DICTA_WHISPERSUP_STUB_REFUSE_TO_BIND"); d == "1" {
		// Sleep forever without binding — supervisor's readiness probe
		// must time out.
		select {}
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	if d := os.Getenv("DICTA_WHISPERSUP_STUB_CRASH_AFTER_MS"); d != "" {
		ms, err := strconv.Atoi(d)
		if err == nil && ms > 0 {
			go func() {
				time.Sleep(time.Duration(ms) * time.Millisecond)
				os.Exit(99)
			}()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"stub","language":"en","duration":1}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	return srv.Serve(l)
}
