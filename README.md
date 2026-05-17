# dicta

[![CI](https://github.com/matthewjhunter/dicta/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/dicta/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/matthewjhunter/dicta.svg)](https://pkg.go.dev/github.com/matthewjhunter/dicta)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/dicta)](https://goreportcard.com/report/github.com/matthewjhunter/dicta)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A Linux/Wayland-first voice dictation daemon written in pure Go.

dicta is two things:

1. **Type-mode** — press **Pause**, talk, and the daemon types the
   transcribed text into whatever window has focus, committing each
   utterance on VAD silence. Press **Pause** again to stop.
2. **Clip-mode** — press **Scroll Lock**, talk, and a small editable
   panel appears with the cleaned transcript. Press **Enter** to copy
   the buffer to the clipboard, **Shift+Enter** to insert a newline,
   **Esc** to cancel.

There is no PTT, no wakeword, no always-on listening. Capture starts
when you press a key and stops when the session ends.

## Status

Pre-1.0. The full v1 build (phases 1–13 of the design) is functional;
this is the docs phase. Use it, but expect rough edges and please file
issues.

## Why

Speech-to-text is one of the few accessibility tools where Linux still
has gaps. Existing options either depend on commercial cloud APIs,
require Python toolchains and GPU model files, or assume X11. dicta is
a single static Go binary that:

- Runs anywhere Wayland and PipeWire run.
- Talks to any Wyoming-protocol ASR server (faster-whisper et al.) by
  default — no model download in v1.
- Optionally talks to a local `whisper-server` (subprocess-managed),
  or any OpenAI-compatible transcription endpoint.
- Optionally cleans transcripts with any OpenAI-compatible LLM
  (llama.cpp's server, vLLM, OpenAI itself).

## Architecture in one diagram

```
   ┌──────────────┐    ┌──────────────┐    ┌────────────────┐    ┌──────────────┐
   │   Pause /    │ →  │    dictad    │ →  │   asrclient    │ →  │  Wyoming /   │
   │ Scroll Lock  │    │              │    │  (Go module)   │    │  whispercpp/ │
   │ (compositor) │    │  audio + VAD │ ←  │                │ ←  │    OpenAI    │
   └──────────────┘    │  state mach. │    └────────────────┘    └──────────────┘
                       │  control sock│
                       │              │    ┌──────────────┐
                       │              │ →  │   ydotool    │  (type-mode)
                       │              │    └──────────────┘
                       │              │    ┌──────────────┐    ┌──────────────┐
                       │              │ ↔  │ dicta-preview│ →  │   wl-copy    │  (clip-mode)
                       │              │    │   (Gio UI)   │    └──────────────┘
                       └──────────────┘    └──────────────┘
```

`dictad` is the daemon (long-lived). `dicta` is a thin CLI that talks
to the daemon over a Unix socket. `dicta-preview` is the clip-mode
panel, spawned on demand. ydotoold and the ASR backend are external.

## Quick start

### 1. Install build deps

```sh
# Ubuntu / Debian
sudo ./scripts/install-deps-ubuntu.sh

# Fedora
sudo ./scripts/install-deps-fedora.sh

# Arch
sudo ./scripts/install-deps-arch.sh
```

These install: Go 1.24+, the Gio system libraries (Wayland, xkbcommon,
GLES, EGL, libvulkan, libXcursor) for the preview panel, ydotool, and
wl-clipboard.

### 2. Build everything

```sh
task build:all
```

Produces `bin/dictad`, `bin/dicta`, and `bin/dicta-preview`.

### 3. Install into your home directory

```sh
task install:user
```

Installs to `~/.local/bin` and drops the systemd user unit into
`~/.config/systemd/user/`.

### 4. Bring up an ASR backend

The default backend is Wyoming. You can run any Wyoming-compatible
service — most users want
[wyoming-faster-whisper](https://github.com/rhasspy/wyoming-faster-whisper).
A common setup is its Docker image listening on `tcp://localhost:10300`.

Other backends:

- `--asr-backend whispercpp` — dicta supervises a local
  `whisper-server` subprocess. Requires you to install
  `whisper.cpp/whisper-server` and a model.
- `--asr-backend openai` — point at any OpenAI-compatible
  `/v1/audio/transcriptions` endpoint. Requires an API key.

See [CONFIGURATION.md](CONFIGURATION.md) for every flag.

### 5. Configure flags

```sh
systemctl --user edit dictad.service
```

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend wyoming \
    --asr-wyoming-addr tcp://localhost:10300 \
    --preview-binary %h/.local/bin/dicta-preview
```

### 6. Enable and start

```sh
systemctl --user enable --now dictad.service
journalctl --user -u dictad.service -f
```

### 7. Bind compositor shortcuts

| Key | What it does | Command |
|-----|--------------|---------|
| Pause | Toggle type-mode session | `dicta toggle_talk --mode type` |
| Scroll Lock | Toggle clip-mode panel | `dicta toggle_talk --mode clip` |

For GNOME, bind these via `gsettings` (the Settings GUI tries to nudge
you toward chord shortcuts; bypassing it lets you use unmodified
single keys). For Sway/Hyprland/KDE, bind in the compositor config.

## Optional: LLM cleanup

Off by default. To enable in clip-mode (the preview panel will display
cleaned text the user can still edit before pressing Enter):

```ini
ExecStart=%h/.local/bin/dictad \
    ... \
    --cleanup-enabled \
    --cleanup-endpoint http://my-llama-server.lan:8080/v1 \
    --cleanup-model qwen3-7b-instruct
```

The mechanical system prompt is a code constant (cannot be templated
by user input). Cleanup is **only** invoked in clip-mode; type-mode
always sends the raw transcript to ydotool.

## Optional: audit log (debug mode)

Off by default. JSONL transcripts (and optionally WAV captures) under
`$XDG_DATA_HOME/dicta/YYYY-MM-DD/`:

```ini
ExecStart=%h/.local/bin/dictad \
    ... \
    --audit-enabled \
    --audit-keep-audio \
    --audit-retention-days 7
```

Both `--audit-enabled` and `--audit-keep-audio` are required to capture
audio. Both default off because both are sensitive by definition.

## Hotkey philosophy

v1 ships exactly two compositor bindings (D17 in the design doc): Pause
for type-mode, Scroll Lock for clip-mode. There is no global commit or
cancel hotkey — clip-mode commits via panel-local Enter and type-mode
commits per-utterance via VAD silence. PTT (push-to-talk) and wakeword
are **out of scope for v1** and are tracked in §14 of the design doc.

## Documentation

- [dicta-design.md](dicta-design.md) — the design spec (v0.2). Read
  this before opening a non-trivial PR.
- [CONFIGURATION.md](CONFIGURATION.md) — every flag.
- [SECURITY.md](SECURITY.md) — security model and the code paths that
  enforce it.
- [packaging/systemd/README.md](packaging/systemd/README.md) — systemd
  unit install and override patterns.

## Building from source (no Taskfile)

```sh
# Daemon + CLI (pure Go, static)
CGO_ENABLED=0 go build -o bin/dictad ./cmd/dictad
CGO_ENABLED=0 go build -o bin/dicta ./cmd/dicta

# Preview panel (CGo, Wayland)
go build -tags nox11 -o bin/dicta-preview ./cmd/dicta-preview
```

The daemon and CLI MUST build with `CGO_ENABLED=0` (D13). The
`MemoryDenyWriteExecute=true` flag in the systemd unit relies on this.

## Testing

```sh
task test       # unit tests
task test:race  # with race detector + goleak
task vet        # go vet
task check      # all of the above
```

`internal/control` ships a fuzz target for the wire-protocol parser:

```sh
go test -fuzz=FuzzCommandUnmarshal -fuzztime=1m ./internal/control
```

## Contributing

The design doc's §13 lists the open decision points; everything else
is locked. If you want to change a locked decision, file an issue
explaining why before writing code — these were deliberate.

Bugs, typos, packaging contributions: PRs welcome.

## License

Apache-2.0 — see [LICENSE](LICENSE).
