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
	"github.com/matthewjhunter/dicta/internal/audit"
	"github.com/matthewjhunter/dicta/internal/cleanup"
	"github.com/matthewjhunter/dicta/internal/control"
	"github.com/matthewjhunter/dicta/internal/dispatch"
	"github.com/matthewjhunter/dicta/internal/whispersup"
)

const version = "0.1.0-dev"

func main() {
	socketFlag := flag.String("socket", "", "control socket path (default: $XDG_RUNTIME_DIR/dicta.sock)")
	audioMonitorFlag := flag.Bool("audio-monitor", false, "phase-3 dev mode: continuously capture audio and expose VAD stats via `dicta status`")
	audioBackendFlag := flag.String("audio-backend", "auto", "audio capture backend: pipewire | pulse | auto")
	audioDeviceFlag := flag.String("audio-device", "", "audio source name (PipeWire node or pulse source); empty = system default")
	asrBackendFlag := flag.String("asr-backend", "", "asr backend: wyoming | whispercpp | openai (empty = disabled)")
	asrAddrFlag := flag.String("asr-wyoming-addr", "tcp://localhost:10300", "wyoming server address (host:port or tcp://host:port)")
	whisperBinaryFlag := flag.String("whispercpp-binary", "/usr/local/bin/whisper-server", "whisper-server binary path (whispercpp backend)")
	whisperModelFlag := flag.String("whispercpp-model", "", "whisper-server model path (whispercpp backend)")
	whisperPortFlag := flag.Int("whispercpp-port", 0, "whisper-server bind port (0 = pick free ephemeral)")
	whisperThreadsFlag := flag.Int("whispercpp-threads", 0, "whisper-server thread count (0 = auto)")
	openaiKeyEnvFlag := flag.String("asr-openai-key-env", "OPENAI_API_KEY", "env var holding the openai API key (openai backend)")
	openaiEndpointFlag := flag.String("asr-openai-endpoint", "", "openai transcription endpoint URL (empty = asrclient default)")
	openaiModelFlag := flag.String("asr-openai-model", "", "openai transcription model name (empty = asrclient default)")
	openaiSkipVerifyFlag := flag.Bool("asr-openai-tls-skip-verify", false, "DANGEROUS: skip TLS certificate verification on the openai endpoint (testing only)")
	ydotoolBinaryFlag := flag.String("ydotool-binary", "/usr/bin/ydotool", "ydotool binary path (must be on the allowlist)")
	ydotoolSocketFlag := flag.String("ydotool-socket", "", "ydotoold socket path (empty = let ydotool pick its default)")
	typeChunkSizeFlag := flag.Int("type-chunk-size", 200, "ydotool dispatch chunk size in characters")
	typeChunkDelayFlag := flag.Duration("type-chunk-delay", 20*time.Millisecond, "delay between ydotool chunks")
	typeKeyDelayFlag := flag.Duration("type-key-delay", 60*time.Millisecond, "ydotool --key-delay between keystrokes (0 = ydotool default 12ms; lower values risk dropped spaces under hardened systemd scheduling)")
	audioCuesFlag := flag.Bool("audio-cues", true, "play short tones on session open/close")
	wlCopyBinaryFlag := flag.String("wl-copy-binary", "/usr/bin/wl-copy", "wl-copy binary path (clip-mode commit)")
	previewBinaryFlag := flag.String("preview-binary", "", "dicta-preview binary path (empty = clip-mode disabled)")
	cleanupEnabledFlag := flag.Bool("cleanup-enabled", false, "enable LLM cleanup of clip-mode transcripts (default off; v1 ships disabled)")
	cleanupEndpointFlag := flag.String("cleanup-endpoint", "", "OpenAI-protocol base URL for cleanup (e.g. http://strix-halo.lan:8080/v1)")
	cleanupKeyEnvFlag := flag.String("cleanup-api-key-env", "DICTA_LLM_KEY", "env var holding the cleanup bearer token (empty = no auth header)")
	cleanupModelFlag := flag.String("cleanup-model", "", "cleanup model name (e.g. qwen3-7b-instruct); required when --cleanup-enabled")
	cleanupTimeoutFlag := flag.Duration("cleanup-timeout", 10*time.Second, "per-call timeout for cleanup HTTP requests")
	cleanupMaxTokensFlag := flag.Int("cleanup-max-tokens", 2048, "max_tokens cap on cleanup responses")
	cleanupTLSSkipFlag := flag.Bool("cleanup-tls-skip-verify", false, "DANGEROUS: skip TLS verification on the cleanup endpoint (testing only)")
	auditEnabledFlag := flag.Bool("audit-enabled", false, "DEBUG: write per-utterance JSONL records to disk (default off; transcripts are sensitive)")
	auditKeepAudioFlag := flag.Bool("audit-keep-audio", false, "DEBUG: also write per-utterance WAV captures (requires --audit-enabled; default off)")
	auditDirectoryFlag := flag.String("audit-directory", "", "audit data directory (empty = $XDG_DATA_HOME/dicta)")
	auditRetentionDaysFlag := flag.Int("audit-retention-days", 0, "delete audit day-dirs older than N days (0 = keep forever)")
	vadCalibrateFlag := flag.Duration("vad-calibrate", 500*time.Millisecond, "VAD noise-floor calibration window at session open; raise if you tend to start speaking immediately")
	vadHangoverFlag := flag.Duration("vad-hangover", 800*time.Millisecond, "VAD continuous-silence threshold to declare end-of-utterance")
	vadMarginDBFlag := flag.Float64("vad-margin-db", 6, "VAD speech threshold = noise floor + this many dB; raise if ambient noise causes spurious speech detection")
	vadMaxUtteranceFlag := flag.Duration("vad-max-utterance", 10*time.Second, "hard cap on a single utterance's duration; force-emits and starts a new chunk on overflow (0 disables)")
	vadMinSpeechMSFlag := flag.Duration("vad-min-speech-ms", 400*time.Millisecond, "minimum raw-energy speech duration per utterance (in 80ms frames); shorter blips are dropped before reaching ASR to suppress Whisper hallucinations like \"Thank you\" (0 disables)")
	asrTranscribeTimeoutFlag := flag.Duration("asr-transcribe-timeout", 30*time.Second, "per-utterance Transcribe deadline; raise for slow CPUs / large models")
	asrMaxConcurrentFlag := flag.Int("asr-max-concurrent", 2, "max concurrent in-flight Transcribe calls; utterances beyond this are dropped with a WARN")
	stripDisfluenciesFlag := flag.String("strip-disfluencies", defaultDisfluencies, "comma-separated list of filler tokens to strip from every transcript (case-insensitive, word-boundary matched); empty string disables stripping. Trailing ellipsis runs (\"...\", \"…\") are always trimmed regardless.")
	unmuteToDictateFlag := flag.Bool("unmute-to-dictate", false, "open type-mode automatically when the configured mic transitions from muted to unmuted, and close on the reverse. Detects mute via all-zero PCM frames; only works on mics whose touch-mute streams literal zeros (e.g. MXL AC-44). Requires --audio-monitor.")
	unmuteDebounceFlag := flag.Duration("unmute-to-dictate-debounce", 1*time.Second, "minimum duration a mute-state change must persist before the watcher fires a transition. Lower for snappier response, raise if you see spurious toggles. Rounded to whole 80ms frames internally.")
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

	bus := newEventBus(logger)
	handler := &stubHandler{version: version, bus: bus}

	var audioMon *audioMonitor
	if *audioMonitorFlag {
		audioMon = newAudioMonitor(logger, audio.CaptureConfig{
			Backend: audio.CaptureBackend(*audioBackendFlag),
			Device:  *audioDeviceFlag,
		}, audio.VADConfig{
			Calibrate: *vadCalibrateFlag,
			Hangover:  *vadHangoverFlag,
			MarginDB:  *vadMarginDBFlag,
		})
		// 16 kHz mono int16 LE = 32 000 bytes / s. Convert the
		// duration flag to a byte cap. 0s disables the cap.
		if *vadMaxUtteranceFlag > 0 {
			capBytes := int(vadMaxUtteranceFlag.Seconds() * float64(audio.SampleRateHz*audio.SampleWidth))
			audioMon.SetMaxUtterance(capBytes)
		}
		if *vadMinSpeechMSFlag > 0 {
			frames := max(int(*vadMinSpeechMSFlag/audio.FrameDuration), 1)
			audioMon.SetMinRawSpeechFrames(frames)
		}
	}

	if *asrBackendFlag != "" {
		asrCfg := asr.Config{
			Backend: *asrBackendFlag,
			Wyoming: asr.WyomingConfig{Addr: *asrAddrFlag},
			OpenAI: asr.OpenAIConfig{
				APIKeyEnv:             *openaiKeyEnvFlag,
				Endpoint:              *openaiEndpointFlag,
				Model:                 *openaiModelFlag,
				InsecureSkipTLSVerify: *openaiSkipVerifyFlag,
			},
		}

		// whispercpp requires the supervisor to start whisper-server and
		// hand its endpoint to asr.Select.
		if *asrBackendFlag == "whispercpp" {
			sup, err := whispersup.New(whispersup.Config{
				Binary:    *whisperBinaryFlag,
				ModelPath: *whisperModelFlag,
				Port:      *whisperPortFlag,
				Threads:   *whisperThreadsFlag,
			}, logger)
			if err != nil {
				logger.Error("whispersup.new", "err", err)
				os.Exit(1)
			}
			if err := sup.Start(ctx); err != nil {
				logger.Error("whispersup.start", "err", err)
				os.Exit(1)
			}
			defer sup.Stop()

			waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
			if err := sup.WaitReady(waitCtx); err != nil {
				waitCancel()
				logger.Error("whispersup.wait_ready", "err", err, "last_err", sup.LastError())
				os.Exit(1)
			}
			waitCancel()
			asrCfg.WhisperCpp = asr.WhisperCppConfig{Endpoint: sup.Endpoint()}
			logger.Info("whispersup ready", "endpoint", sup.Endpoint())
		}

		backend, err := asr.Select(asrCfg)
		if err != nil {
			logger.Error("asr.select", "err", err, "backend", *asrBackendFlag)
			os.Exit(1)
		}
		disfluencyRE := compileDisfluencyRE(*stripDisfluenciesFlag)
		if disfluencyRE != nil {
			logger.Info("asr disfluency strip enabled", "tokens", *stripDisfluenciesFlag)
		} else {
			logger.Info("asr disfluency strip disabled (empty list)")
		}
		asrMon := newASRMonitor(logger, backend, asrMonitorConfig{
			BackendName:       *asrBackendFlag,
			HealthInterval:    10 * time.Second,
			TranscribeTimeout: *asrTranscribeTimeoutFlag,
			MaxConcurrent:     *asrMaxConcurrentFlag,
			DisfluencyRE:      disfluencyRE,
		})
		asrMon.Start(ctx)
		defer asrMon.Stop()
		defer backend.Close()
		handler.asr = asrMon

		logger.Info("asr-monitor started", "backend", *asrBackendFlag)
	}

	// Type-mode session orchestrator (phase 7). Requires both audio
	// capture and an ASR backend; without either, ToggleTalk replies
	// not_implemented and the daemon stays useful only for status.
	var sess *session
	if audioMon != nil && handler.asr != nil {
		if err := ensureExecutable("ydotool-binary", *ydotoolBinaryFlag); err != nil {
			logger.Error("dispatch.typer", "err", err)
			os.Exit(1)
		}
		typer, err := dispatch.NewSubprocessTyper(dispatch.TyperConfig{
			Binary:     *ydotoolBinaryFlag,
			Socket:     *ydotoolSocketFlag,
			ChunkSize:  *typeChunkSizeFlag,
			ChunkDelay: *typeChunkDelayFlag,
			KeyDelay:   *typeKeyDelayFlag,
			Logger:     logger,
		})
		if err != nil {
			logger.Error("dispatch.typer", "err", err)
			os.Exit(1)
		}
		var cuer audio.Cuer = audio.NewSubprocessCuer(audio.CueConfig{
			Disabled: !*audioCuesFlag,
		})

		// Clip-mode wiring is optional: if --preview-binary is empty,
		// the clipper and preview controller stay nil and Toggle("clip")
		// returns ErrClipNotConfigured.
		var clipper dispatch.Clipper
		var preview previewController
		if *previewBinaryFlag != "" {
			if err := ensureExecutable("wl-copy-binary", *wlCopyBinaryFlag); err != nil {
				logger.Error("dispatch.clipper", "err", err)
				os.Exit(1)
			}
			if err := ensureExecutable("preview-binary", *previewBinaryFlag); err != nil {
				logger.Error("preview.controller", "err", err)
				os.Exit(1)
			}
			cl, err := dispatch.NewSubprocessClipper(dispatch.ClipperConfig{
				Binary: *wlCopyBinaryFlag,
				Logger: logger,
			})
			if err != nil {
				logger.Error("dispatch.clipper", "err", err)
				os.Exit(1)
			}
			clipper = cl

			pp, err := newPreviewProc(previewConfig{
				Binary: *previewBinaryFlag,
				Socket: socketPath,
				Logger: logger,
			})
			if err != nil {
				logger.Error("preview.controller", "err", err)
				os.Exit(1)
			}
			preview = pp
			logger.Info("clip-mode enabled", "preview", *previewBinaryFlag, "wl_copy", *wlCopyBinaryFlag)
		}

		cleaner, err := cleanup.New(cleanup.Config{
			Enabled:               *cleanupEnabledFlag,
			Endpoint:              *cleanupEndpointFlag,
			APIKeyEnv:             *cleanupKeyEnvFlag,
			Model:                 *cleanupModelFlag,
			Timeout:               *cleanupTimeoutFlag,
			MaxTokens:             *cleanupMaxTokensFlag,
			InsecureSkipTLSVerify: *cleanupTLSSkipFlag,
		}, logger)
		if err != nil {
			logger.Error("cleanup.new", "err", err)
			os.Exit(1)
		}
		if *cleanupEnabledFlag {
			logger.Info("cleanup enabled", "endpoint", *cleanupEndpointFlag, "model", *cleanupModelFlag)
		}

		auditW, err := audit.New(audit.Config{
			Enabled:       *auditEnabledFlag,
			Directory:     *auditDirectoryFlag,
			KeepAudio:     *auditKeepAudioFlag,
			RetentionDays: *auditRetentionDaysFlag,
		}, logger)
		if err != nil {
			logger.Error("audit.new", "err", err)
			os.Exit(1)
		}
		defer auditW.Close()
		if *auditEnabledFlag {
			logger.Warn("audit enabled — per-utterance transcripts will be written to disk",
				"directory", *auditDirectoryFlag,
				"keep_audio", *auditKeepAudioFlag,
				"retention_days", *auditRetentionDaysFlag)
		}

		sess = newSession(logger, typer, clipper, cuer, handler.asr, audioMon.VAD(), bus, preview, cleaner, auditW, ctx)
		audioMon.onUtterance = sess.OnUtterance
		handler.session = sess
		logger.Info("session orchestrator ready", "ydotool", *ydotoolBinaryFlag, "audio_cues", *audioCuesFlag)

		// Unmute-to-dictate watcher. Requires audioMon to be running so
		// it has a frame stream to consume; --audio-monitor is the
		// existing gate for that. The watcher tracks transitions only —
		// startup state seeds without firing, so the daemon doesn't open
		// a session just because the mic happens to be unmuted at boot.
		if *unmuteToDictateFlag {
			debounceFrames := max(int(*unmuteDebounceFlag/audio.FrameDuration), 1)
			watcher := newMuteWatcher(logger, sess, debounceFrames)
			audioMon.onFrame = watcher.OnFrame
			logger.Info("unmute-to-dictate enabled",
				"debounce_frames", debounceFrames,
				"debounce_ms", debounceFrames*int(audio.FrameMS))
		}
	} else if audioMon != nil {
		// Audio without ASR: keep utterance counting alive, but no
		// dispatch path is available.
		audioMon.onUtterance = func(pcm []byte) { _ = pcm }
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

	// Phase 12: signal-handling spec verification (§12 of design doc).
	// SIGTERM cancels ctx, srv.Serve returns, and we land here. If a
	// session was open at the time of the signal, close it explicitly
	// so the audit log records the close, the close cue plays, and
	// for clip-mode the preview panel receives an explicit SIGTERM via
	// preview.Kill (idempotent with the cmd.Cancel path that has
	// already fired on ctx cancellation).
	//
	// Use context.Background() here, NOT ctx — ctx is already done from
	// the signal, and the cuer's Play would short-circuit through the
	// ctx.Err() path without producing a tone. A fresh context lets the
	// shutdown path complete its side effects.
	if sess != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := sess.Shutdown(shutdownCtx); err != nil {
			logger.Warn("session.shutdown", "err", err)
		}
		shutdownCancel()
	}
	logger.Info("dictad stopped")
}
