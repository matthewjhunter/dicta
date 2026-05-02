// Command dictad is the long-lived dicta daemon. It owns audio capture,
// the ASR client (and supervises whisper-server when that backend is
// selected), LLM cleanup, output dispatch, and the control socket. It
// spawns dicta-preview on demand for clip-mode.
//
// dictad is the only place permitted to import multiple internal/ packages
// — the mode state machine (open/close type session, spawn/kill panel,
// enforce D6 mutual exclusion) lives here.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/matthewjhunter/dicta/internal/control"
)

const version = "0.1.0-dev"

func main() {
	socketFlag := flag.String("socket", "", "control socket path (default: $XDG_RUNTIME_DIR/dicta.sock)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	socketPath := *socketFlag
	if socketPath == "" {
		p, err := control.DefaultSocketPath()
		if err != nil {
			logger.Error("resolve socket path", "err", err)
			os.Exit(1)
		}
		socketPath = p
	}

	srv, err := control.Listen(socketPath, &stubHandler{version: version}, func(format string, args ...any) {
		logger.Warn("control", "msg", fmt.Sprintf(format, args...))
	})
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("dictad started", "version", version, "socket", socketPath)

	if err := srv.Serve(ctx); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
	logger.Info("dictad stopped")
}
