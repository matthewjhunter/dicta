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
	"time"

	"github.com/matthewjhunter/dicta/internal/asr"
	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/control"
)

const version = "0.1.0-dev"

func main() {
	socketFlag := flag.String("socket", "", "control socket path (default: $XDG_RUNTIME_DIR/dicta.sock)")
	audioMonitorFlag := flag.Bool("audio-monitor", false, "phase-3 dev mode: continuously capture audio and expose VAD stats via `dicta status`")
	audioBackendFlag := flag.String("audio-backend", "auto", "audio capture backend: pipewire | pulse | auto")
	audioDeviceFlag := flag.String("audio-device", "", "audio source name (PipeWire node or pulse source); empty = system default")
	asrBackendFlag := flag.String("asr-backend", "", "asr backend: wyoming | whispercpp | openai (empty = disabled)")
	asrAddrFlag := flag.String("asr-wyoming-addr", "tcp://localhost:10300", "wyoming server address (host:port or tcp://host:port)")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	handler := &stubHandler{version: version}

	var audioMon *audioMonitor
	if *audioMonitorFlag {
		audioMon = newAudioMonitor(logger, audio.CaptureConfig{
			Backend: audio.CaptureBackend(*audioBackendFlag),
			Device:  *audioDeviceFlag,
		}, audio.VADConfig{})
	}

	if *asrBackendFlag != "" {
		backend, err := asr.Select(asr.Config{
			Backend: *asrBackendFlag,
			Wyoming: asr.WyomingConfig{Addr: *asrAddrFlag},
		})
		if err != nil {
			logger.Error("asr.select", "err", err, "backend", *asrBackendFlag)
			os.Exit(1)
		}
		asrMon := newASRMonitor(logger, backend, asrMonitorConfig{
			BackendName:       *asrBackendFlag,
			HealthInterval:    10 * time.Second,
			TranscribeTimeout: 30 * time.Second,
		})
		asrMon.Start(ctx)
		defer asrMon.Stop()
		defer backend.Close()
		handler.asr = asrMon

		if audioMon != nil {
			audioMon.onUtterance = asrMon.OnUtterance
		}
		logger.Info("asr-monitor started", "backend", *asrBackendFlag, "addr", *asrAddrFlag)
	}

	if audioMon != nil {
		if err := audioMon.Start(ctx); err != nil {
			logger.Error("audio.start", "err", err)
			os.Exit(1)
		}
		defer audioMon.Stop()
		handler.audio = audioMon
		logger.Info("audio-monitor started", "backend", audioMon.Snapshot().Backend)
	}

	srv, err := control.Listen(socketPath, handler, func(format string, args ...any) {
		logger.Warn("control", "msg", fmt.Sprintf(format, args...))
	})
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	logger.Info("dictad started", "version", version, "socket", socketPath, "audio_monitor", *audioMonitorFlag)

	if err := srv.Serve(ctx); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
	logger.Info("dictad stopped")
}
