# dicta -- Design Document

**Status:** v0.2 design, pre-implementation
**Target implementer:** Claude Code
**Owner:** matthewjhunter
**License:** MIT

---

## 1. Purpose

`dicta` is a Linux/Wayland-first voice dictation daemon. It transcribes
spoken input via a pluggable ASR backend (Wyoming, `whisper.cpp`, or
OpenAI-protocol HTTP) and dispatches the result to one of two output paths:

1. **Type-mode** -- VAD-segmented utterances are synthesized as keystrokes
   into the focused application via `ydotool`. Single-line: no newline is
   ever sent. The session stays open between utterances so phrases
   accumulate on the same destination line until explicitly closed.
2. **Clip-mode** -- a sidecar preview panel (`dicta-preview`) listens,
   displays a live editable transcript, and on commit writes the cleaned
   (and possibly user-edited) text to the Wayland clipboard via `wl-copy`.

Activation is single-key, compositor-bound, taps only:

- **Pause** -- toggle the type-mode session. Tap to open; tap again to
  close. Within an open session, VAD silence commits each utterance via
  `ydotool` and the session stays open for the next phrase.
- **Scroll Lock** -- toggle the clip-mode panel. Tap to spawn
  `dicta-preview`; tap again (or close the panel) to dismiss. The panel
  scopes the Enter key as the commit signal so global Enter is never
  bound and remains usable for normal text input.

Push-to-talk and continuous-listen wakeword are **not** v1 modes -- see
D5 and 5.3.

The name is medieval Latin / scholastic for *"the art of composition by
dictation."*

## 2. Non-goals

- Windows or macOS support.
- X11 support (we will not maintain `xdotool`/`xclip` paths). X11 sessions can
  use `soupawhisper` or similar; this daemon assumes Wayland.
- A GUI configuration tool. CLI + config file for the daemon. The clip-mode
  preview panel is a single-purpose review UI in its own sidecar binary,
  not a general configuration surface.
- Realtime streaming transcription (utterance-at-a-time only, v1).
- Push-to-talk (hold-a-key) activation. Pause+VAD covers the same use case
  with lower keystroke load. See D5.
- Wakeword / continuous listen. Deferred to v2 as a separate sidecar; see
  5.3.
- Source dependency on `majordomo`. That project is on hiatus; `dicta`
  must not import or vendor it. The Wyoming wire protocol and OpenAI-
  compatible HTTP transcription clients live in a separate
  `github.com/matthewjhunter/asrclient` module that `dicta` imports;
  the `IsQuiet` PCM helper and mic-cue tones are reimplemented fresh
  in-tree under `internal/audio`. See D16.

## 3. Constraints and decisions (locked)

| ID  | Decision | Rationale |
|-----|----------|-----------|
| D1  | Audio capture: spawn `pw-record` (PipeWire) or `parec` (PulseAudio compat) as subprocess | Avoids CGo/PipeWire binding complexity; robust; PipeWire is the daily-driver baseline on modern Ubuntu |
| D2  | ASR backend: pluggable via the `asrclient.Backend` interface from `github.com/matthewjhunter/asrclient`. Three impls in v1, all selected by config: `wyoming` (default; `asrclient/wyoming.Client` to a Wyoming-protocol STT server such as `wyoming-faster-whisper`), `whispercpp` (`asrclient/whispercpp.Client` to a daemon-supervised `whisper-server` subprocess on loopback), `openai` (`asrclient/openai.Client` to a user-managed OpenAI-protocol HTTP endpoint). The whisper-server *lifecycle* (spawn, port discovery, restart-on-crash, /health gating) lives in `dicta`; the `asrclient/whispercpp` client is just HTTP. | Wyoming is the lingua franca of the local-voice-AI ecosystem; defaulting to it lets `dicta` share an STT server with anything else on the network that speaks Wyoming. `whispercpp` is the standalone option for users without a Wyoming setup; `openai` is for cloud/remote. Protocol code lives in `asrclient` (reusable, single API for all three); subprocess supervision is dicta-specific (configuration, logging, fault recovery integrated with the rest of the daemon). All three avoid CGo and Python. |
| D3  | Wakeword: deferred to v2. v2 ships two modes -- `wyoming` (default, pure Go, TCP to a Wyoming wakeword server such as `wyoming-openwakeword`, reusing the `asrclient/wyoming` low-level wire surface) and `embedded` (build-tagged `onnx`, CGo via `yalue/onnxruntime_go`, openWakeWord pipeline). Default builds remain pure Go. | Pure-Go default keeps D13 intact; the embedded path is opt-in for users who want a single binary. The embedded implementation is *reimplemented*, not vendored -- see D16. |
| D4  | LLM cleanup: OpenAI-protocol HTTP client, configurable endpoint, no default URL | Works with `olla`, `llama.cpp` server, vLLM, Anthropic-compatible gateways, OpenAI itself |
| D5  | Activation is compositor-bound single-key shortcuts only: **Pause** for type-mode, **Scroll Lock** for clip-mode. No PTT in v1: no evdev sidecar, no `input` group requirement, no second systemd unit. | PTT requires hold-and-release semantics that compositor shortcuts can't deliver, which would force an evdev sidecar. It also costs more keystrokes than tap-toggle, which contradicts the owner's ergonomic constraint (D17). Pause+VAD provides the same controlled-session-with-per-utterance-commit shape with lower keystroke load. |
| D6  | Mode arbitration: type-mode and clip-mode are mutually exclusive. Activating one while the other is active first closes the active mode, then opens the new one. | Avoids ambiguous output routing; matches "one mic, one destination at a time." |
| D7  | Output review is mode-specific. Type-mode commits each utterance immediately on VAD silence; the open Pause session is the user's input gate, not a per-utterance review step. Clip-mode routes through the preview panel, where the user reviews and edits before pressing Enter to commit. | Type-mode users want phrases to land as they speak; clip-mode users want to compose and review. Different ergonomics, different flows. |
| D8  | Storage: flat files (JSON Lines metadata + WAV audio) in dated directories. memstore optional, future. | No DB dependency in v1 |
| D9  | Glossary: none in v1. Hooks present for memstore-driven glossary in v2. | Ship something useful before adding ML cleverness |
| D10 | Profiles: single config, mode chosen by hotkey | Simpler |
| D11 | Service model: `systemd --user`, hardened unit. One service unit (`dictad.service`); `dicta-preview` is launched on demand by the daemon, not as a separate persistent service. | Consistent with homelab style; minimizes always-running surface. |
| D12 | Safety boundaries are mode-specific. **Type-mode**: the open Pause session is the safety boundary. The user explicitly opens and explicitly closes; only within an open session can VAD silence trigger keystroke synthesis. **Clip-mode**: the preview panel is the pending buffer. Nothing reaches the clipboard until the user presses Enter inside the panel. Type-mode cleaned text has `\n` stripped defensively before dispatch. | Bounds the terminal-shell-injection footgun without a separate per-utterance commit gesture. Reviewing in the panel is the natural review step for cleaned text. |
| D13 | The daemon (`dictad`) and CLI (`dicta`) are pure Go, no CGo, no Python -- `CGO_ENABLED=0`, fully static. The `dicta-preview` sidecar is allowed to use any UI toolkit (GTK4, Qt, gioui, etc.) since it is a separate binary on a separate process boundary; the daemon's static-binary and `MemoryDenyWriteExecute` properties are unaffected. | Single static-binary distribution for the security-relevant components; no Python supply chain in the audio/ASR/dispatch path; smaller attack surface for the long-running daemon. The preview panel is short-lived and user-launched. |
| D14 | Audio capture: `pw-record`/`parec` subprocess in v1 (honors D13). A build-tagged PortAudio backend (`gordonklaus/portaudio`) is reserved as a v2 escape hatch if subprocess proves flaky in practice. | Subprocess loses some determinism (chunk timing, cleaner device enumeration) but keeps the daemon pure Go. The `internal/audio.Capture` interface is shaped so a PortAudio impl can be added later without touching call sites. |
| D15 | Audio constants locked: 16 kHz, mono, int16 little-endian, 80 ms / 1280-sample chunks. | Standard across the local-voice-AI ecosystem; what Wyoming STT/wake servers expect; what openWakeWord models consume. Lets a future `internal/wake` consume `dicta` audio buffers with no resampling or re-chunking. |
| D16 | No source dependency on `majordomo` (project on hiatus). The Wyoming wire protocol and the OpenAI-compatible HTTP transcription clients live in a separate `github.com/matthewjhunter/asrclient` module that `dicta` imports; subprocess lifecycle (whisper-server) stays in `dicta`. PCM-format helpers (`IsQuiet`, mic-cue tones) are reimplemented fresh in-tree under `internal/audio`. No `replace` directive against `majordomo`, no `git submodule`, no in-tree vendor of `majordomo` source. | Avoids tying `dicta`'s build to a paused project. Splits protocol from lifecycle so the protocol surface is reusable; `asrclient` is shaped to be the single Backend API for any future voice consumer (including `majordomo` if it un-pauses). |
| D17 | Hotkey defaults and recommendations are single-key only. v1 default bindings: **Pause** for type-mode toggle, **Scroll Lock** for clip-mode toggle. No modifier-chord bindings in shipped examples, default config, or documentation. Users may still bind chords on their own; `dicta` itself does not constrain compositor bindings. | Owner ergonomic constraint: multi-key chords are physically costly. Pause and Scroll Lock are vestigial keys on modern keyboards, almost never collide with other bindings, and a tap is the lowest possible keystroke load. |

## 4. Architecture overview

```
                                ┌───────────────────┐
                                │     dictad        │
                                │   (pure Go)       │
                                └─────────┬─────────┘
                                          │
       ┌──────────────────┬───────────────┼──────────────────┬──────────────────┐
       │                  │               │                  │                  │
       ▼                  ▼               ▼                  ▼                  ▼
┌──────────────┐   ┌──────────────┐  ┌──────────────┐  ┌────────────────┐ ┌──────────────┐
│  Unix socket │   │  Audio capt. │  │  asrclient   │  │  LLM cleanup   │ │ dicta-preview│
│  control API │   │  (pw-record) │  │  (external   │  │  (OpenAI HTTP) │ │  (sidecar)   │
│  + event sub │   │  16k/mono/   │  │   module):   │  │                │ │  GUI panel   │
│              │   │  S16LE/80ms  │  │ wyoming/     │  │                │ │  for clip    │
│              │   │              │  │ whispercpp/  │  │                │ │              │
│              │   │              │  │ openai       │  │                │ │              │
└──────┬───────┘   └──────┬───────┘  └──────┬───────┘  └──────┬─────────┘ └──────┬───────┘
       │                  │                 │                 │                  │
       └──────────────────┴────────┬────────┴─────────────────┴──────────────────┘
                                   │
                                   ▼
                          ┌────────────────┐
                          │ Output dispatch │
                          │ - ydotool       │
                          │ - wl-copy       │
                          │ - notify-send   │
                          └─────────────────┘
```

### 4.1 Process topology

- **`dictad`** -- main daemon. Long-lived. Owns audio capture, ASR client
  (and `whisper-server` supervision when the `whispercpp` backend is
  configured), LLM cleanup client, output dispatch, and the control
  socket. Spawns `dicta-preview` on demand when clip-mode opens. Pure Go,
  no CGo, no Python.
- **`dicta`** -- CLI client. Sends one command to `dictad` over the Unix
  socket, reads one response, exits. Used by compositor shortcuts and
  for scripting. Pure Go.
- **`dicta-preview`** -- clip-mode preview panel sidecar, launched by
  the daemon. Connects to the daemon's control socket, subscribes to
  transcript events, displays them in an editable text area, and sends
  `commit` (Enter) or `cancel` (Esc / window-close) back to the daemon.
  UI toolkit choice deferred to implementation; the daemon's pure-Go
  guarantee is unaffected by the panel's deps per D13.
- **`ydotoold`** -- third-party. Started independently (likely via its own
  systemd service). The daemon talks to it via its socket.

### 4.2 Hard module boundaries

The following packages must NOT import each other:

```
internal/audio       capture, VAD, ringbuffer, mic-cue tones, IsQuiet
internal/asr         thin selector: reads config, returns an asrclient.Backend
internal/whispersup  supervises the local whisper-server subprocess
                     (only used when [asr] backend = "whispercpp")
internal/cleanup     LLM cleanup client (OpenAI-protocol)
internal/dispatch    ydotool + wl-copy + notify-send wrappers (no policy)
internal/control     Unix socket server: command protocol + event subscriptions
internal/config      typed config loading
internal/audit       JSONL + WAV writer

(v2) internal/wake   wakeword detection. Out of scope for v1; see 5.3.
```

Cross-cutting (allowed everywhere): `internal/log`, `internal/errors`.

The Wyoming wire protocol and OpenAI-compatible HTTP transcription
clients live OUTSIDE this repository in
`github.com/matthewjhunter/asrclient` and are imported as a normal Go
module dependency (D16). dicta's `internal/asr` is a thin selector that
reads `[asr] backend = ...` and returns an `asrclient.Backend`;
`internal/whispersup` exists only to spawn and watch the local
`whisper-server` subprocess when the whispercpp backend is configured.

The mode state machine (open/close type session, spawn/kill preview panel,
route transcripts to ydotool vs. clipboard, enforce D6 mutual exclusion)
lives in `cmd/dictad/main.go`. The orchestrator is the *only* place that
imports multiple internal packages.

`cmd/dicta-preview/` is a sibling binary in the same Go module. It connects
to `dictad` only via the control socket and does NOT import any
`internal/` package or `asrclient` -- the socket protocol is its only API
surface.

## 5. Component specifications

### 5.1 `internal/audio`

**Responsibilities**

- Spawn `pw-record` (preferred) or `parec` (fallback) as a subprocess capturing
  16 kHz mono S16LE PCM to a pipe.
- **Frame format is locked (D15)**: 16 kHz, mono, int16 little-endian,
  80 ms chunks = 1280 samples = 2560 bytes per frame (matches the
  constants exposed by `asrclient` for the same values). Producers and
  consumers across `internal/audio`, `internal/asr`, the
  `asrclient` module, and a future `internal/wake` all assume this
  exact shape with no resampling.
- Provide a `Capture` interface with `Start(ctx) <-chan Frame`, `Stop() error`.
- Maintain a ring buffer of the last N seconds (configurable, default 30s) for
  silence-detect lookback (and v2 wakeword pre-roll).
- Mic-cue tones: short pure-Go-synthesized chirps for mic-open and
  mic-close events, played through the audio output. Sine-wave generator
  with linear-ramp attack/release to avoid clicks. ~100 lines, pure Go.
  Configurable, default on. Fired on type-session open/close and on
  preview-panel open/close.
- `IsQuiet(pcm []byte, threshold float64) bool` helper for normalized
  RMS-vs-threshold checks. ~20 lines, pure Go, in-package.
- Voice Activity Detection (VAD). **Type-mode commits depend on this
  being correct**, so calibration is load-bearing. Pure-Go,
  energy-based, in-package -- no external library, no CGo. Spec:
  - 20ms windows of S16LE PCM -> RMS energy.
  - Adaptive noise floor: calibrate over the first 500ms of each opened
    session (assumed silence; user is not yet talking right after the
    Pause tap). Maintain a slow-moving floor estimate via exponential
    moving average over windows classified as silence.
  - Speech threshold: floor + margin (configurable, default 6 dB).
  - Per-utterance hangover: end-of-utterance fires after `hangover_ms`
    continuous silence (configurable, default 800ms). In type-mode the
    fire commits the buffered phrase via ydotool and re-arms within the
    same session.
  - On hardware change or long silence, recalibrate.
  This is intentionally simpler than webrtc-vad / silero. Adequate for
  dictation in typical desk environments. The `VAD` interface stays
  stable so a heavier implementation can replace it later if needed.

**Device selection**

- Source of truth at session start (in priority order):
  1. The runtime-state file at `$XDG_STATE_HOME/dicta/state.toml`,
     written by `dicta mic select`. Field: `audio.device` =
     PipeWire **node name** (e.g.
     `alsa_input.usb-Logitech_USB_Headset-00.mono-fallback`).
     Node *names* are persistent across reboots; node *IDs* are not,
     so this code stores names only.
  2. `[audio].device` in the user config file, if non-empty.
  3. PipeWire's current default source (the `device = ""` case).
- If the selected device is missing at session start, the daemon logs
  `WARN dicta.audio.device-missing name=...` and falls back to the
  system default. The daemon stays healthy; the audit log records the
  device actually used. This is the right shape for a user-session
  daemon -- hard-failing on a missing USB headset would be hostile.
- Hot-plug re-routing while a session is open is **not** in v1: the
  daemon captures from whichever node was current at session start.
  If the user changes their default mid-session, the change takes
  effect at the next session-open. Documented as a v1 limitation;
  v1.x improvement.

**State file format**

```toml
# $XDG_STATE_HOME/dicta/state.toml
[audio]
device = "alsa_input.usb-Logitech_USB_Headset-00.mono-fallback"
```

`dicta mic select --reset` deletes the `[audio].device` line, returning
selection to the user-config or system-default chain.

**Interface**

```go
type Capture interface {
    Start(ctx context.Context) (<-chan Frame, error)
    Stop() error
    Backend() string // "pipewire" | "pulse"
}

type Frame struct {
    PCM       []byte    // S16LE mono
    Timestamp time.Time
}

type VAD interface {
    IsSpeech(frame Frame) bool
}
```

**Configuration**

```toml
[audio]
backend = "auto"         # "pipewire" | "pulse" | "auto"
sample_rate = 16000
channels = 1
device = ""              # empty = follow PipeWire default at session start
ringbuffer_seconds = 30

[audio.vad]
margin_db = 6            # speech threshold = noise floor + this
hangover_ms = 800        # silence required to declare end-of-utterance
calibrate_ms = 500       # initial silence assumed for noise-floor calibration
```

### 5.2 `internal/asr` (selector) and `internal/whispersup` (lifecycle)

**Responsibilities**

The `Backend` interface, the three protocol implementations, and the
shared `Options` / `Transcript` / `Segment` types live in
`github.com/matthewjhunter/asrclient` -- not in this repository. dicta
imports that module.

dicta provides two thin layers on top:

- `internal/asr.Select(cfg)` returns an `asrclient.Backend` constructed
  from the configured `[asr]` block: `asrclient/wyoming.NewClient(addr)`,
  `asrclient/whispercpp.NewClient(opts...)`, or
  `asrclient/openai.NewClient(apiKey, opts...)`.
- `internal/whispersup` supervises the local `whisper-server` subprocess
  when the whispercpp backend is selected: spawn at startup with
  port-discovery, exponential-backoff restart on crash, `/health`
  gating. The daemon does not advertise ASR readiness on its control
  socket until the supervisor reports green. The supervisor owns the
  binary path and CLI flags from `[asr.whispercpp]` config; the
  `asrclient/whispercpp.Client` is told the resulting endpoint URL.

**Backend interface (provided by asrclient, repeated here for context):**

```go
type Backend interface {
    Transcribe(ctx context.Context, audio []byte, opts Options) (Transcript, error)
    Healthy(ctx context.Context) error
    Close() error
}

type Options struct {
    Language      string  // "" = auto
    InitialPrompt string  // optional bias text
    Temperature   float32
}

type Transcript struct {
    Text     string
    Language string
    Duration time.Duration
    Segments []Segment // optional, with timestamps
}
```

**Configuration**

```toml
[asr]
backend = "wyoming"          # "wyoming" | "whispercpp" | "openai"

[asr.wyoming]
addr = "tcp://localhost:10300"   # standard wyoming-faster-whisper port
reconnect_backoff_initial_ms = 1000
reconnect_backoff_max_ms = 30000

[asr.whispercpp]
binary = "/usr/local/bin/whisper-server"
model_path = "${XDG_DATA_HOME}/dicta/models/ggml-base.en.bin"
host = "127.0.0.1"
port = 0                     # 0 = pick free ephemeral port at startup
threads = 0                  # 0 = auto: min(runtime.NumCPU()/2, 8). whisper.cpp inference plateaus past ~8 threads.
extra_args = []              # passthrough flags for whisper-server (e.g. GPU/device selection: "-ngl", "--device", Vulkan/ROCm/CUDA toggles per local build)
startup_timeout_seconds = 30
restart_backoff_initial_ms = 500
restart_backoff_max_ms = 30000

[asr.openai]
endpoint = "http://localhost:8080/v1/audio/transcriptions"
api_key_env = "DICTA_ASR_KEY"
model = "whisper-1"
tls_verify = true
timeout_seconds = 30
```

For `whispercpp`, the daemon prefers an ephemeral port (`port = 0`) discovered
by spawning `whisper-server` and reading the bound port from its log/stderr,
or by binding a free port itself and passing it to the server.

Health-check protocol per backend:
- `wyoming` -- TCP connect plus a periodic `describe` event round-trip.
- `whispercpp` -- `GET /health` against the supervised server.
- `openai` -- HEAD probe on the transcription endpoint, or a cached
  result of the most recent transcription (probes are billable on
  some providers).

A failed health probe transitions the backend to `unhealthy`, which causes
`Transcribe` to fail fast with a clear error instead of hanging.

### 5.3 `internal/wake` (deferred to v2)

**Status:** Out of scope for v1. Pause+VAD covers the v1 use case.

**Why deferred:** D13 (pure Go, no CGo, no Python) constrains the default
build. v2 ships two modes behind a `Mode` knob:

- `wyoming` (default) -- TCP client to a Wyoming-protocol wakeword server
  (e.g. `wyoming-openwakeword`). Pure Go, no CGo. Reuses the
  low-level `asrclient/wyoming` event/wire surface (Event, Conn,
  ReadEvent, WriteEvent) -- detect events live on the same protocol.
  Auto-reconnect with exponential backoff. This is the intended default
  and keeps daemon builds pure Go.
- `embedded` -- build-tagged `//go:build onnx`. Loads `melspectrogram.onnx`
  + `embedding_model.onnx` + per-keyword scoring models from a config
  directory using `yalue/onnxruntime_go` (CGo). Reimplements the
  openWakeWord pipeline (mel -> embedding -> per-keyword scoring) with
  RMS-based silence skip and a ~2s post-detection refractory window.
  Without the `onnx` build tag a stub returns "embedded mode
  unavailable; use wyoming."

Model files are independent of the inference code: any
openWakeWord-trained model (`*.onnx`) drops into the configured models
directory.

The control protocol (5.6) reserves space for v2 wake commands but the v1
daemon does not implement them.

### 5.4 `internal/cleanup`

**Responsibilities**

- Send raw transcript to a configured OpenAI-protocol chat completions
  endpoint with a constrained system prompt.
- Two prompt profiles, selectable per dispatch:
  - **mechanical** (used by clip-mode by default): fix punctuation,
    capitalization, obvious homophones; remove disfluencies; do not
    change wording, structure, or word choice; do not add or remove
    content.
  - **passthrough** (used by type-mode): no LLM call. Returns input
    unchanged.

The mechanical prompt is a constant in code; configurable override via
config file.

**Interface**

```go
type Cleaner interface {
    Clean(ctx context.Context, raw string, profile Profile) (string, error)
}

type Profile string

const (
    ProfileMechanical  Profile = "mechanical"
    ProfilePassthrough Profile = "passthrough"
)
```

**Configuration**

```toml
[cleanup]
enabled = true
endpoint = ""                # required if enabled; e.g. "http://strix-halo.lan:8080/v1"
api_key_env = "DICTA_LLM_KEY"
model = "qwen3-7b-instruct"
timeout_seconds = 10
max_tokens = 2048
```

If `enabled = false` or `endpoint = ""`, all cleanup calls return input
unchanged. Daemon must start cleanly with cleanup disabled.

The mechanical system prompt MUST be defined in a constant and not be
runtime-templated by user input. Future: glossary slot.

### 5.5 `internal/dispatch`

**Responsibilities**

- `Type(text string) error` -- invoke `ydotool type --` via the `ydotoold`
  socket. Strips `\n` defensively before dispatch (D12); type-mode
  buffers should not contain newlines, but the strip is cheap insurance
  against ASR/cleanup misbehavior. Handles long text by chunking; injects
  a small inter-chunk delay.
- `Clip(text string) error` -- pipe text into `wl-copy`. No newline strip.
- `Notify(title, body string, urgency Urgency) error` -- invoke `notify-send`.
- All three must tolerate the underlying tool being absent and log clearly.

This package is dumb wrappers only -- no session state, no commit-gate
logic. The state machine that decides *when* to call `Type` or `Clip`
lives in the orchestrator (`cmd/dictad/main.go`) per the module-boundary
rule.

**Configuration**

```toml
[dispatch]
ydotool_socket = "/run/user/1000/.ydotool_socket"  # default; auto-detected
type_chunk_size = 200       # characters per chunk
type_chunk_delay_ms = 20
clipboard_tool = "wl-copy"
audio_cues = true           # short chirp on session/panel open and close
audio_cue_open_freq_hz = 880
audio_cue_close_freq_hz = 660
audio_cue_duration_ms = 80
```

### 5.6 `internal/control`

**Responsibilities**

- Unix socket server at `$XDG_RUNTIME_DIR/dicta.sock` (mode 0600).
- Newline-delimited JSON protocol. Each accepted connection is one of:
  - **Command channel** -- one command per line, one response per line.
    Default mode for new connections.
  - **Event channel** -- after a `subscribe` command, the connection is
    locked to event-stream mode; the daemon pushes JSON events. Further
    commands on this connection are rejected.
- Authentication: socket is user-owned + 0600. No further auth in v1.

**Commands (v1)**

```json
{"cmd": "toggle_talk", "mode": "type"}    // Pause: open/close type session
{"cmd": "toggle_talk", "mode": "clip"}    // Scroll Lock: open/close panel
{"cmd": "commit", "text": "..."}          // panel: send text to clipboard
{"cmd": "cancel"}                          // panel: discard buffer, close panel
{"cmd": "subscribe", "events": ["transcript", "session_state"]}
{"cmd": "mic_list"}                       // enumerate audio sources
{"cmd": "mic_select", "name": "alsa_input...", "reset": false}
{"cmd": "status"}
{"cmd": "shutdown"}
```

`commit.text` carries the panel's edited text -- the daemon uses that
verbatim for `wl-copy` rather than its own internal buffer. The user's
edits are authoritative.

`wake_*` commands are reserved for v2 and rejected by the v1 daemon with a
clear `not_implemented` error code.

The protocol enforces a maximum line length (default 64 KiB) to bound DoS
risk on the socket. Lines exceeding the cap are rejected and the connection
is closed.

**Responses**

```json
{"ok": true, "data": {...}}
{"ok": false, "error": "...", "code": "..."}
```

`mic_list` returns `data` as a JSON array of objects:

```json
[{"name": "alsa_input...", "description": "USB Headset", "default": true,  "selected": false},
 {"name": "alsa_input...", "description": "Built-in Mic",  "default": false, "selected": true}]
```

`mic_select` with `reset: true` clears the runtime-state selection and
returns to the user-config or system-default chain. With a `name` field,
it writes the runtime state. The new selection takes effect at the next
session-open (no live re-routing in v1; see §5.1).

The `dicta` CLI renders `mic_list` as a table when stdout is a TTY, and
emits the raw JSON array otherwise -- same shape as `dicta status`.

**Events** (pushed on subscribed connections)

```json
{"event": "transcript", "data": {"text": "...", "final": true|false, "utterance_id": "..."}}
{"event": "session_state", "data": {"mode": "type|clip|none", "open": true|false}}
```

Partial transcripts (`final: false`) may be emitted by backends that
support streaming; v1's three backends transcribe utterance-at-a-time, so
in practice only `final: true` events fire in v1. The shape is reserved
for v2 streaming.

The CLI client `dicta` is a thin wrapper that connects on the command
channel, writes one command, reads one response, exits. `dicta-preview`
opens two connections: one for `subscribe`, one for commands.

### 5.7 `dicta-preview` (clip-mode panel)

**Responsibilities**

- Single-purpose GUI window, launched by the daemon when clip-mode opens.
- Connects to the daemon's control socket twice: one event channel
  (subscribed to `transcript` and `session_state`), one command channel.
- Displays an editable multi-line text area. Appends each `final: true`
  transcript event to the area, separated by spaces (or by newline if the
  user has explicitly inserted one).
- Key bindings inside the window:
  - **Enter** -- send `{"cmd": "commit", "text": <buffer>}`, then close.
  - **Shift+Enter** -- insert a literal newline at the cursor.
  - **Esc** or window close -- send `{"cmd": "cancel"}`, then close.
- Should always-on-top and focused on launch so Enter goes to the panel,
  not whatever app was previously focused.

**Lifecycle**

- Spawned by `dictad` via `exec.Command` when `toggle_talk --mode clip`
  fires and clip-mode is currently closed. Args include the path to the
  control socket.
- Killed by `dictad` (SIGTERM) when:
  - User toggles clip-mode off (Scroll Lock again).
  - Type-mode is opened (D6 mutual exclusion).
  - Daemon shuts down.
- The panel itself exits cleanly after sending `commit` or `cancel`.

**UI toolkit: Gio (`gioui.org`).**

Gio is the chosen toolkit. Idiomatic Go API, Wayland-first on Linux,
mature `widget.Editor` for the multi-line text area. Gio requires CGo,
gcc, pkg-config, and dev packages for Wayland, xkbcommon, GLES, EGL,
and libXcursor on Linux build hosts; the resulting `dicta-preview`
binary is dynamically linked to those system libraries at runtime. This
is permitted because the panel is a separate process boundary from the
daemon (D13) -- `dictad` and `dicta` remain pure-Go static binaries.

Build the panel with the `nox11` Gio build tag in v1; the project is
Wayland-first (D5, §2) and dropping X11 shrinks the binary and the dep
surface.

Implementation lift is small: one `widget.Editor` configured for
multi-line, a key handler for Enter/Shift+Enter/Esc, and an event
goroutine that consumes `transcript` events from the subscribed control
socket and appends to the editor's buffer.

### 5.8 `internal/audit`

**Responsibilities**

- For every transcription, write a JSONL record and (configurably) the WAV
  audio to a dated directory under `$XDG_DATA_HOME/dicta/`.
- Record fields: timestamp, mode (`type`|`clip`), raw transcript, cleaned
  transcript, panel-edited final text (clip-mode only), ASR backend, ASR
  latency, cleanup latency, output target hint, dispatch status
  (committed/cancelled/timeout).
- Daily rotation. Configurable retention (default: keep forever).

**Configuration**

```toml
[audit]
enabled = true
directory = ""               # empty = $XDG_DATA_HOME/dicta
keep_audio = true
audio_format = "wav"
retention_days = 0           # 0 = forever
```

### 5.9 `internal/config`

**Responsibilities**

- Typed config struct, loaded from TOML.
- Search order: `--config` flag > `$XDG_CONFIG_HOME/dicta/config.toml` > `/etc/dicta/config.toml`.
- Validate at load time. Refuse to start on invalid config.
- No hot reload in v1.

## 6. Hotkey configuration (user-side)

Compositor-bound shortcuts invoke `dicta` with appropriate args. v1 ships
exactly two recommended bindings (D17):

| Key | Action | Command |
|-----|--------|---------|
| Pause | Toggle type-mode session | `dicta toggle_talk --mode type` |
| Scroll Lock | Toggle clip-mode panel | `dicta toggle_talk --mode clip` |

There is no global commit hotkey (clip-mode commits via panel-local
Enter; type-mode commits per-utterance via VAD silence). There is no
global cancel hotkey (clip-mode cancels via panel-local Esc; type-mode
cancels by pressing Pause to close the session).

GNOME custom shortcuts can bind unmodified single keys; if the Settings
GUI nudges you toward chords, set the shortcut via `gsettings` directly.

## 7. systemd

### 7.1 `dictad.service` (user)

```ini
[Unit]
Description=dicta voice dictation daemon
After=graphical-session.target pipewire.service
Wants=pipewire.service

[Service]
Type=notify
ExecStart=/usr/local/bin/dictad
Restart=on-failure
RestartSec=3

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.local/share/dicta %t
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true         # daemon is pure Go, no JIT or interpreter
RestrictRealtime=true
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=default.target
```

`dicta-preview` is launched on demand by the daemon (`exec.Command`) and
inherits no special hardening; the panel's privilege footprint is just
"a user-process GUI app." It is not a separate systemd unit.

### 7.2 ydotoold

Assumed installed and managed separately via its upstream-provided systemd
unit or distro package. The daemon does not start it.

## 8. Security considerations

1. **Input synthesis is privileged.** ydotoold + the ydotool socket give
   the daemon keystroke-injection capability. This is intrinsic to the
   dispatch goal. Mitigations:
   - ydotoold runs under the user, not root.
   - The daemon synthesizes keystrokes only inside an open type-mode
     session. Closed sessions cannot dispatch. The Pause toggle is the
     user's input-gate.
   - Type-mode cleaned text has `\n` stripped defensively before
     dispatch (D12), to prevent newline injection into shell prompts or
     other newline-sensitive contexts.
   - With PTT removed (D5), no component requires `input`-group
     membership; the kernel-evdev attack surface is gone.
2. **Audio capture is on-demand only in v1.** Capture starts when a
   session opens and stops when it closes. No always-on listening (that
   would require wakeword, deferred to v2). Audit log retention is
   user-configurable.
3. **LLM cleanup leaks transcript content** to the configured endpoint. If
   `endpoint` points outside the LAN, the user is sending dictation to that
   service. Default config ships with cleanup disabled; user must
   explicitly enable and configure.
4. **Clip-mode never types into the focused app.** All clip-mode output
   goes to `wl-copy`; the user pastes it themselves. The shell-injection
   class of bugs is structurally absent.
5. **Whisper backend supply chain.** `whisper-server` is a C++ binary; the
   user installs it from their distro or builds from source. The model
   files (`ggml-*.bin`) are downloaded by the user, not by the daemon.
   Document recommended sources (upstream `whisper.cpp` releases). The
   daemon does NOT auto-download models in v1.
6. **Pure-Go daemon (D13).** `dictad` and `dicta` contain no CGo and no
   embedded interpreter. This shrinks the in-process attack surface and
   lets `MemoryDenyWriteExecute=true` apply cleanly. `dicta-preview` is
   a separate process; its toolkit may use CGo without affecting the
   daemon's hardening.
7. **Config is TOML, parsed by `pelletier/go-toml/v2`.** No shell-out from
   config values without explicit allowlist. Path values are validated
   against a set of allowed prefixes. Subprocess argv lists are built
   from typed config values, never via shell.
8. **Socket auth.** v1 relies on filesystem permissions (0600, user-owned).
   Adequate for single-user systems. The protocol enforces a max line
   length to bound DoS risk (5.6).
9. **TLS verification.** All HTTP clients (LLM cleanup, OpenAI ASR) verify
   certificates by default. The `tls_verify = false` knob exists only for
   local-LAN testing and emits a startup WARN when enabled.

## 9. Error handling and observability

- Structured logging via `log/slog`. JSON to stdout when run under systemd, text
  to stderr when run interactively (TTY-detected).
- Levels: DEBUG, INFO, WARN, ERROR. Default INFO.
- Each utterance gets a request ID; ID flows through audio -> ASR -> cleanup
  -> dispatch logs.
- Health exposed via `dicta status` returning JSON: backend availability,
  ydotool reachability, current mode (`type`/`clip`/`none`), session
  open/closed, last transcription summary (no transcript text, just
  length and timestamp).
- No metrics endpoint in v1. Prometheus scraping is a v2 nice-to-have.

## 10. Testing

- **Unit tests** for every package, table-driven. Hard requirement: each
  internal package has its own test file.
- **Integration tests** for ASR backends use a small canned WAV fixture and a
  stub server. No real model invocation in CI.
- **Fuzz tests** on:
  - Control socket protocol parser (commands and events).
  - Config TOML parser.
  - LLM cleanup response parser (defensive -- endpoint may misbehave).
- **VAD calibration tests.** Type-mode commits depend on VAD; tests
  feed canned WAV fixtures (clean dictation, noisy office, music
  background) and assert that hangover_ms / margin_db produce
  reasonable utterance boundaries. Calibration regressions should fail
  CI.
- **Race detector** must pass: `go test -race ./...`.
- **CI**: GitHub Actions, matrix on Go 1.22+ (or whatever current stable +1 is).

## 11. Build and release

- Single Go module: `github.com/matthewjhunter/dicta`.
- License: MIT.
- `make` targets: `build`, `test`, `lint`, `install`, `package-deb`,
  `package-rpm`, `package-arch`.
- Reproducible builds: `-trimpath -ldflags="-s -w -buildid="`.
- **Hard build target for daemon and CLI: pure Go, no CGo.** `dictad` and
  `dicta` build with `CGO_ENABLED=0` and produce fully static
  executables. CI enforces this with `CGO_ENABLED=0 go build` and
  `file` checks for "statically linked." Any dependency that requires
  CGo is rejected at code review for these two binaries.
- **`dicta-preview` builds against Gio (`gioui.org`)** with `CGO_ENABLED=1`
  and the `nox11` build tag. Build-host dev packages on Debian/Ubuntu:
  `gcc`, `pkg-config`, `libwayland-dev`, `libxkbcommon-dev`,
  `libgles2-mesa-dev`, `libegl1-mesa-dev`, `libxcursor-dev`. Runtime
  dynamic deps: `libwayland-client.so.0`, `libxkbcommon.so.0`,
  `libGLESv2.so.2`, `libEGL.so.1`, `libxcursor.so.1`. Distro packages
  must declare these as panel-only runtime deps; the daemon and CLI
  packages do not depend on them.
- Pinned third-party deps for daemon (all pure Go): `pelletier/go-toml/v2`,
  `coreos/go-systemd/v22/daemon`, `uber-go/goleak` (test-only),
  `github.com/matthewjhunter/asrclient` (Wyoming wire protocol +
  OpenAI-compatible HTTP transcription clients; see D16).
- The `asrclient` module is owned alongside `dicta` and pinned to a
  specific module version in `go.mod`. Per D16: NO source dependency
  on `majordomo`.
- SBOM via `cyclonedx-gomod`.

## 12. Implementation order

The implementer should build in this order. Each phase produces something
demonstrable before the next phase begins.

1. **Skeleton.** Module, package layout, config loading, `cmd/dictad`
   and `cmd/dicta` stubs, control socket round-trip (commands only, with
   line-length cap), systemd unit (unhardened first, hardened in
   phase 10).
2. **Bootstrap `asrclient`.** Separate `github.com/matthewjhunter/asrclient`
   module with `Backend` interface, audio-format constants, and Wyoming
   wire protocol + Backend impl (table-driven tests against captured
   wire bytes; integration test gated on a live server). Done before
   any ASR work in `dicta` so the default backend has a working
   transport from day one. `dicta` imports it as a normal dependency.
3. **Audio capture + VAD.** `pw-record` subprocess emitting locked-format
   80ms / 1280-sample frames, frame channel, ring buffer, pure-Go
   energy VAD with the §5.1 calibration spec, in-tree `IsQuiet`
   helper, mic-cue tone generator. Manual test via `dicta status`
   showing audio frames flowing and VAD speech/silence transitions.
4. **ASR -- `wyoming` backend (default).** dicta's `internal/asr.Select`
   returns an `asrclient/wyoming.Client` for `[asr] backend = "wyoming"`.
   Stream audio, await transcript, exponential-backoff reconnect on
   transport errors. Manual test: capture a clip and print transcript
   to log against a local `wyoming-faster-whisper`.
5. **ASR -- `whispercpp` backend.** `internal/whispersup` supervises
   the `whisper-server` subprocess (port discovery, `/health` gating,
   restart-on-crash). `internal/asr.Select` wires
   `asrclient/whispercpp.NewClient(WithEndpoint(supervisedURL))` once
   the supervisor reports green. Selectable via config.
6. **ASR -- `openai` backend.** dicta's `internal/asr.Select` returns an
   `asrclient/openai.NewClient(apiKey, ...)` for `[asr] backend = "openai"`.
   No subprocess lifecycle; TLS verify default-on (per asrclient defaults).
7. **Type-mode session + dispatch.** Orchestrator state machine in
   `cmd/dictad/main.go` for the type-mode session (open on Pause command,
   per-utterance commit on VAD silence, close on Pause command). `Type()`
   in `internal/dispatch` with `\n` strip. Audio cues fire on
   session open/close.
8. **Control-socket event subscription.** Add `subscribe` command and
   `transcript` / `session_state` event push. Required before the
   preview panel can consume live transcripts.
9. **`dicta-preview` sidecar.** Choose toolkit (decision in §13),
   implement the editable panel, wire up two-connection client
   (subscribe + commands), launch from daemon on `toggle_talk --mode clip`.
10. **LLM cleanup.** OpenAI-protocol client, mechanical prompt,
    integration with clip-mode dispatch (panel sees cleaned text,
    user can still edit).
11. **Audit.** JSONL writer, WAV writer, retention.
12. **Hardening pass.** systemd unit hardening, fuzz tests,
    race-detector clean, goroutine-leak (`goleak`) checks in tests,
    security review checklist, signal-handling spec verified
    (SIGTERM closes any open type session and any open preview panel
    cleanly).
13. **Docs.** README, CONFIGURATION.md, SECURITY.md, install scripts
    for Ubuntu/Fedora/Arch.

Wakeword (`internal/wake`) is v2 and not part of this sequence.

## 13. Open decision points (defer to implementation)

- **Notification icon set.** Desktop-notification icons. Use FreeDesktop
  names.

## 14. Out of scope (v1)

- Wakeword detection (the entire `internal/wake` package -- see 5.3).
- Push-to-talk activation (D5).
- Streaming partial transcripts within a single utterance.
- Multi-language detection per-utterance.
- Per-window-class profile switching.
- memstore integration.
- Glossary injection.
- Speaker diarization.

## 15. Glossary

- **VAD** -- voice activity detection
- **ASR** -- automatic speech recognition
- **session** (type-mode) -- the period between Pause-tap-open and
  Pause-tap-close, during which VAD silence triggers per-utterance
  commits via ydotool
- **panel** (clip-mode) -- the `dicta-preview` sidecar GUI window where
  transcripts accumulate and are committed to the clipboard via Enter
- **ydotool** -- userland input event injector via `/dev/uinput`
- **Wyoming** -- JSON-header + binary-payload TCP wire protocol for local
  voice services (STT, wakeword, TTS), originated by Home Assistant.
  `dicta`'s implementation is consumed from the
  `github.com/matthewjhunter/asrclient` module under `asrclient/wyoming`.

---

*End of design doc.*
