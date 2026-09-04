# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

**Implemented and running.** The daemon (`cmd/dictad`), CLI (`cmd/dicta`), and clip-mode panel (`cmd/dicta-preview`) all exist, with a Go module (`go.mod`), a full test suite, a `Taskfile.yml`, GitHub Actions CI, and systemd packaging under `packaging/`. `dicta-design.md` (v0.2) remains the authoritative design and most of its decisions are locked — read the relevant section before changing behavior — but it describes the target, not a greenfield. The `--unmute-to-dictate` mute-detection subsystem has its own design doc, `mute-source-design.md` (decision **D18**), which the main design doc's D-list does not cover; read it before touching anything under `internal/mute`.

### Build / test / lint

Driven by `Taskfile.yml` (run `task` to list targets):

- `task build` — build `dictad` + `dicta` (pure Go, static). `task build:all` adds `dicta-preview` (needs CGo + Wayland/GLES dev headers).
- `task test` — unit tests; `task test:race` adds the race detector (needs CGo). Or `go test ./...` directly.
- `task vet` / `task lint` (golangci-lint) / `task fmt` / `task vuln` (govulncheck).
- `task check` — the full CI gate (vet, fmt, lint, test, test:race, vuln). Run before pushing.
- `task install:user` installs all three binaries into `~/.local/bin`; `task restart` restarts the `dictad` user service to pick up a new build.

## What dicta is

A Linux/Wayland-first voice dictation daemon written in pure Go. Two activation paths, both single-key compositor shortcuts:

- **Pause** → toggles a **type-mode session**. While the session is open, VAD silence commits each utterance via `ydotool` and the session stays open for the next phrase. Single-line: no `\n` is ever sent. Pause again closes the session.
- **Scroll Lock** → toggles the **clip-mode panel** (`dicta-preview` sidecar GUI). Live transcript appears in an editable text area; **Enter** in the panel commits to the Wayland clipboard via `wl-copy`, **Shift+Enter** inserts a literal newline, **Esc** cancels.

ASR is pluggable: Wyoming (TCP, default), `whisper.cpp` (supervised subprocess), OpenAI-protocol HTTP. LLM cleanup runs on clip-mode text only (mechanical prompt, user-edits-after). Wakeword and PTT are **not v1** — do not implement either.

There is also a **third, optional, default-off activation path**: `--unmute-to-dictate` (D18, `mute-source-design.md`). When enabled, the daemon watches the configured mic's mute state and opens/closes a type-mode session on unmute/mute — the mic's hardware mute button becomes the toggle. This does **not** contradict D5/D17: those govern the compositor *bindings dicta ships*; the unmute path is an opt-in feature gated behind a flag, off by default, with its own security rationale (§3 of the mute doc). It is always-listening when on, by design — do not "fix" idle capture as a bug.

## Locked decisions that constrain implementation

These are non-negotiable without re-opening the design. The IDs map to §3 of the design doc.

- **D5 — Activation is compositor-bound single-key shortcuts only.** Pause = type-mode toggle, Scroll Lock = clip-mode panel toggle. **No PTT in v1**: no evdev sidecar binary, no `internal/evdev` package, no `input`-group requirement, no second systemd unit.
- **D6 — Type-mode and clip-mode are mutually exclusive.** Activating one while the other is open closes the active mode first.
- **D12 — Mode-specific safety boundaries.** Type-mode: the open Pause session IS the safety boundary; only within an open session can VAD silence trigger ydotool. Clip-mode: the preview panel IS the pending buffer; nothing reaches `wl-copy` until Enter is pressed inside the panel. Type-mode dispatch strips `\n` defensively before ydotool to prevent newline injection.
- **D13 — Daemon and CLI are pure Go, no CGo, no Python.** `dictad` and `dicta` build with `CGO_ENABLED=0` and produce static binaries. The `dicta-preview` sidecar is on a separate process boundary and may use any UI toolkit (GTK4, Qt6, gioui.org); D13 only constrains the daemon and CLI.
- **D15 — Audio frame format is locked: 16 kHz, mono, int16 LE, 80 ms / 1280-sample / 2560-byte chunks.** Every producer and consumer across `internal/audio`, `internal/asr`, the `asrclient` module, and a future `internal/wake` assumes this exact shape. No resampling, no re-chunking. The same constants are exported by `asrclient` (SampleRateHz, FrameBytes, etc.) — use those in code that crosses the module boundary.
- **D16 — No source dependency on `majordomo`.** That project is on hiatus. Do not import, `replace`, vendor, or git-submodule it. The Wyoming wire protocol and OpenAI-compatible HTTP transcription clients live in `github.com/matthewjhunter/asrclient` (sibling repo at `~/git/matthewjhunter/asrclient`); dicta consumes that module as a normal Go dependency. PCM-format helpers (`IsQuiet`, mic-cue tones) are reimplemented fresh in-tree under `internal/audio`.
- **D2 — Three ASR backends in v1, all from `asrclient`**: `asrclient/wyoming.Client` (default, TCP), `asrclient/whispercpp.Client` (HTTP to a dicta-supervised local `whisper-server`), `asrclient/openai.Client` (HTTPS to a user-managed endpoint). Subprocess *lifecycle* for whisper-server (spawn, port discovery, restart-on-crash, /health gating) lives in dicta's `internal/whispersup` — `asrclient/whispercpp` is just the HTTP client, kept lifecycle-free so the module stays consumer-agnostic.
- **D3 — Wakeword is deferred to v2.** Do not start `internal/wake` work. The control protocol reserves `wake_*` but v1 must respond `not_implemented`.
- **D17 — Single-key hotkey defaults only.** v1 ships exactly two compositor bindings: Pause (type) and Scroll Lock (clip). No global commit or cancel hotkeys — those are panel-local in clip-mode and don't exist in type-mode (Pause itself closes the session). No modifier chords in shipped examples, default config, or docs.

## Module boundaries (hard)

The packages below must NOT import each other. The mode state machine (open/close type session, spawn/kill panel, enforce D6) lives in `cmd/dictad/main.go`, which is the *only* place that imports multiple `internal/` packages. `internal/dispatch` is dumb wrappers — no session state, no commit-gate logic.

```
internal/audio       capture, VAD, ringbuffer, mic-cue tones, IsQuiet
internal/asr         thin selector returning an asrclient.Transcriber per [asr] config;
                     also owns the `dicta check` fixture + comparison (check.go)
internal/whispersup  whisper-server subprocess supervisor (whispercpp backend only)
internal/cleanup     LLM cleanup client (OpenAI-protocol)
internal/dispatch    ydotool + wl-copy + notify-send wrappers (no policy)
internal/control     Unix socket server: command channel + event subscriptions
internal/config      typed TOML config loading
internal/audit       JSONL + WAV writer
internal/mute        pluggable mute.Source for --unmute-to-dictate (D18);
                     pcmzero/ + pipewire/ subpackages are the two backends
```

`internal/log` and `internal/errors` are cross-cutting and may be imported anywhere.

The unmute-to-dictate **watcher** (debounce, clip-mode safety, fire-on-transition) lives in `cmd/dictad/mutewatch.go`, not in `internal/mute` — `internal/mute` only *observes* state; the watcher *acts* on it, alongside the mode state machine in `cmd/dictad`.

`proto/` is a public package holding the control-protocol wire types (`Command`, `Response`, `Event`, status/event payloads). It is split out from `internal/control` (which type-aliases it) precisely so `cmd/dicta-preview/` can deserialize daemon events without importing `internal/`.

`cmd/dicta-preview/` is a sibling binary that connects to the daemon **only** via the control socket (using `proto/`). It MUST NOT import any `internal/` package — the socket protocol (§5.6) is its sole API.

## Process topology

- `dictad` — long-lived main daemon. Owns audio capture, ASR client (and supervises `whisper-server` when that backend is selected), LLM cleanup, output dispatch, control socket. Spawns `dicta-preview` on demand.
- `dicta` — thin CLI client. One command per invocation, talks to `dictad` over `$XDG_RUNTIME_DIR/dicta.sock` (mode 0600).
- `dicta-preview` — clip-mode panel sidecar, daemon-launched on Scroll Lock. Connects to the control socket twice (one event-subscription channel, one command channel). Built with **Gio** (`gioui.org`) + `nox11` tag; CGo + Wayland/xkbcommon/GLES/EGL/xcursor system libs at build and runtime. The panel is on a separate process boundary so D13's pure-Go constraint does not apply to it.
- `ydotoold` — third-party, managed independently; the daemon talks to its socket.

## Control protocol shape (§5.6)

Newline-delimited JSON. Two channel modes per connection:

- **Command channel** (default): one command per line, one response per line.
- **Event channel** (after `{"cmd":"subscribe", "events":[...]}`): connection locks to event-stream mode; daemon pushes JSON events; further commands rejected.

Commands: `status`, `check` (end-to-end ASR check -- submits an embedded "Hello world" fixture and compares the transcript; seconds, not milliseconds), `toggle_talk` (mode=type|clip), `commit` (carries panel-edited text), `cancel`, `mic_list`, `mic_select`, `subscribe`, `shutdown`. Events: `transcript`, `session_state`. `wake_*` reserved for v2 → `not_implemented`. Max line length 64 KiB. Wire types live in `proto/`; add a new command by extending the daemon-side `control.Handler` interface and the server's dispatch switch.

`commit.text` is authoritative — the daemon uses the panel's edited text verbatim for `wl-copy`, not its own raw transcript buffer.

## Security posture worth re-reading before risky changes

§8 of the design doc governs the security posture; treat it as authoritative. Particular landmines:
- **Subprocess argv lists are built from typed config values, never via shell.** Path values must be validated against an allowlist of prefixes.
- **TLS verification defaults on** for all HTTP clients (LLM cleanup, OpenAI ASR). `tls_verify = false` is a testing-only knob and must emit a startup WARN.
- **The mechanical LLM cleanup system prompt is a code constant** and must not be runtime-templated by user input.
- **Type-mode dispatch strips `\n`** before ydotool to prevent newline injection into shell prompts (D12).
- **`MemoryDenyWriteExecute=true`** in the systemd unit relies on D13 — anything that breaks the daemon's pure-Go property also breaks the hardening.
- **`dicta-preview` runs as an unprivileged user GUI process** without the daemon's hardening; its toolkit deps are NOT subject to D13. Keep the panel's logic minimal — it's a transcript display + edit buffer + three keystrokes, not a place to add features.

## VAD calibration is critical to type-mode

Type-mode commits depend on `internal/audio`'s energy VAD firing correctly. The §5.1 spec (500 ms calibration, 6 dB margin, 800 ms hangover) is the starting point but will need real-world tuning. CI must include calibration regression tests against canned WAV fixtures (clean, noisy office, music background). A flaky VAD means premature commits or never-commits — both are user-facing failures.

## asrclient dependency

The `Transcriber` interface and the three protocol clients (Wyoming, whispercpp, openai) live in `github.com/matthewjhunter/asrclient` (sibling repo at `~/git/matthewjhunter/asrclient`), consumed via `go.mod` (D16). dicta adds only the thin `internal/asr` selector and the `internal/whispersup` lifecycle supervisor on top. Changes to the wire protocols or the `Transcriber` shape belong in that repo, not here.

## When in doubt

- Re-read the relevant section of `dicta-design.md` (or `mute-source-design.md` for anything mute-related) rather than guessing.
- "Open decision points" in §13 are explicitly deferred — flag them and ask before resolving. v0.2 has one: notification icon names.
- Anything in §14 ("Out of scope (v1)") stays out of v1 — including PTT and wakeword.
