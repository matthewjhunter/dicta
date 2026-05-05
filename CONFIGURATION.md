# Configuration

Every CLI flag accepted by `dictad`, organized by subsystem. v1 takes
configuration via flags only; the TOML config-file path described in
the design doc is reserved for a follow-up release.

For background on each subsystem read the matching section of
[dicta-design.md](dicta-design.md).

To set flags under systemd:

```sh
systemctl --user edit dictad.service
```

Then put the override in the `[Service]` block:

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend wyoming \
    ...
```

## Control socket

| Flag | Default | What it does |
|------|---------|--------------|
| `--socket` | `$XDG_RUNTIME_DIR/dicta.sock` | Path the daemon listens on; mode `0600`, user-owned. The CLI uses the same default. |

## Audio

| Flag | Default | What it does |
|------|---------|--------------|
| `--audio-backend` | `auto` | Capture backend: `pipewire`, `pulse`, or `auto`. Auto prefers PipeWire when available. |
| `--audio-device` | (empty = system default) | Source name; PipeWire node name or pulse source. Run `pactl list sources short` to enumerate. |
| `--audio-cues` | `true` | Play short tones on session open/close. Disable on shared microphones to avoid the cue bleeding into capture. |
| `--audio-monitor` | `false` | Dev mode: continuously capture audio and surface VAD stats via `dicta status`. Most users do not need this. |

The audio frame format is locked at 16 kHz mono int16 LE / 80 ms / 1280
samples (D15 in the design doc). No flag changes this.

### VAD tuning

| Flag | Default | What it does |
|------|---------|--------------|
| `--vad-calibrate` | `500ms` | Noise-floor calibration window at session open. Raise if you tend to start speaking immediately. |
| `--vad-hangover` | `800ms` | Continuous silence required to declare end-of-utterance. Lower values commit faster but split mid-sentence pauses; raise to coalesce more aggressively. |
| `--vad-margin-db` | `6` | Speech threshold = noise floor + this many dB. Raise if ambient noise causes spurious speech detection. |
| `--vad-max-utterance` | `10s` | Hard cap on a single utterance's duration. Force-emits and starts a new chunk on overflow (`0` disables). |
| `--vad-min-speech-ms` | `400ms` | Minimum raw-energy speech duration per utterance (rounded to 80 ms frames, so the default is 5 frames). Shorter blips — mic clicks, breath puffs, the cue tone — are dropped before reaching ASR. Whisper-family backends reliably hallucinate "Thank you" / "Thanks for watching" / "you" on those blips. Lower this if very brief one-word utterances ("yes", "no") get dropped; `0` disables the gate. |

The ASR layer also drops a small deny-list of known Whisper artifact
phrases (`thank you`, `thanks for watching`, `you`, `bye`, etc.,
case-insensitive) as a backstop for blips that slip past the VAD gate.
There is no flag for this; the list is intentionally narrow to avoid
suppressing real one-word utterances.

## ASR backend

```
--asr-backend wyoming | whispercpp | openai
```

Empty (the default) disables ASR entirely; the daemon comes up but
`toggle_talk` returns `not_implemented`. Useful for testing the
control plane in isolation.

### Wyoming (default backend)

| Flag | Default | What it does |
|------|---------|--------------|
| `--asr-wyoming-addr` | `tcp://localhost:10300` | Wyoming server address. Accepts bare `host:port` or a `tcp://` URL. |

Wyoming is the easiest path: pull
[wyoming-faster-whisper](https://github.com/rhasspy/wyoming-faster-whisper)
as a Docker image or systemd unit and dicta connects.

### whispercpp (managed local subprocess)

dicta supervises a `whisper-server` process. Lifecycle (spawn, port
discovery, restart-on-crash, /health gating) lives in the daemon;
dicta does NOT auto-download models.

| Flag | Default | What it does |
|------|---------|--------------|
| `--whispercpp-binary` | `/usr/local/bin/whisper-server` | Path to the whisper-server binary. Must be on the allowlist (`/usr/bin`, `/usr/local/bin`, `/opt`). |
| `--whispercpp-model` | (required) | Path to a `ggml-*.bin` model. Must be under `/var/lib/dicta`, `/usr/share/dicta`, or `$XDG_DATA_HOME/dicta`. |
| `--whispercpp-port` | `0` | Bind port (0 = pick a free ephemeral port). |
| `--whispercpp-threads` | `0` | Thread count (0 = auto: half NumCPU, capped at 8). |

### openai (any OpenAI-compatible HTTP endpoint)

| Flag | Default | What it does |
|------|---------|--------------|
| `--asr-openai-key-env` | `OPENAI_API_KEY` | Env var to read the bearer token from. dicta does not enforce key presence — the server decides whether anonymous traffic is accepted. An unset env var produces a request with no `Authorization` header. |
| `--asr-openai-endpoint` | (asrclient default) | Endpoint URL. Empty uses upstream OpenAI. |
| `--asr-openai-model` | (asrclient default) | Model name. Empty uses `whisper-1`. |
| `--asr-openai-tls-skip-verify` | `false` | **DANGEROUS**: skip TLS verification. Testing-only knob; emits a startup WARN. Never set this on a non-LAN target. |

## Output dispatch

### Type-mode (ydotool)

| Flag | Default | What it does |
|------|---------|--------------|
| `--ydotool-binary` | `/usr/bin/ydotool` | Path to `ydotool`. Must be under `/usr/bin`, `/usr/local/bin`, or `/opt`. |
| `--ydotool-socket` | (empty) | `ydotoold` socket path. Empty = let ydotool pick its default. |
| `--type-chunk-size` | `200` | Chunk size in characters per ydotool invocation. Larger = fewer roundtrips, smaller = lower latency on each chunk. |
| `--type-chunk-delay` | `20ms` | Delay between chunks. Some apps drop characters when typed faster than this. |
| `--type-key-delay` | `60ms` | Forwarded to `ydotool --key-delay` (delay between individual keystrokes). ydotool's own default is 12ms; under the daemon's hardened systemd scheduling the kernel uinput buffer drops space keysyms at that rate, so dicta defaults higher. Lower it on machines where the default works to type faster; raise it if you still see dropped characters. `0` falls back to ydotool's 12ms. |

Type-mode strips `\n` defensively before invoking ydotool (D12), to
prevent newline injection into shell prompts.

### Clip-mode (wl-copy + preview panel)

Clip-mode is **disabled** unless `--preview-binary` is set.

| Flag | Default | What it does |
|------|---------|--------------|
| `--preview-binary` | (empty = clip-mode disabled) | Path to `dicta-preview`. Set this to enable clip-mode. |
| `--wl-copy-binary` | `/usr/bin/wl-copy` | Path to `wl-copy`. Must be on the allowlist. |

The preview panel is a separate process (CGo + Wayland) and does NOT
run with the daemon's hardening. See `cmd/dicta-preview/` for the
source and SECURITY.md item 6 for the rationale.

## LLM cleanup (clip-mode only)

Off by default. Cleanup runs on clip-mode transcripts before they hit
the panel; the user can still edit before pressing Enter.

| Flag | Default | What it does |
|------|---------|--------------|
| `--cleanup-enabled` | `false` | Master switch. Off = no HTTP traffic, period. |
| `--cleanup-endpoint` | (empty) | OpenAI-protocol base URL including `/v1`. Required when enabled. Examples: `http://strix-halo.lan:8080/v1`, `https://api.openai.com/v1`. |
| `--cleanup-api-key-env` | `DICTA_LLM_KEY` | Env var holding the bearer token. Empty value = no `Authorization` header (some local servers don't require auth). |
| `--cleanup-model` | (empty) | Model name. Required when enabled. |
| `--cleanup-timeout` | `10s` | Per-call HTTP timeout. Errors fall back to the raw transcript with a WARN — losing punctuation polish is preferable to losing the utterance. |
| `--cleanup-max-tokens` | `2048` | Cap on response length. |
| `--cleanup-tls-skip-verify` | `false` | **DANGEROUS**: same posture as `--asr-openai-tls-skip-verify`. WARN at startup. |

The mechanical system prompt is a code constant (`internal/cleanup`'s
`MechanicalSystemPrompt`) and cannot be templated by user input —
this is a §8 mandate.

## Audit log (debug mode)

**Off by default. Both flags are required to capture audio.**

Transcripts and especially audio are sensitive by definition; the
defaults reflect that. When enabled, the daemon emits a startup WARN
naming the directory so accidental opt-ins are visible in journal.

| Flag | Default | What it does |
|------|---------|--------------|
| `--audit-enabled` | `false` | Write per-utterance JSONL records under the audit directory. |
| `--audit-keep-audio` | `false` | Additionally write per-utterance WAV captures. Requires `--audit-enabled`. |
| `--audit-directory` | (empty = `$XDG_DATA_HOME/dicta`) | Audit data root. Must be under `/var/lib/dicta` or `$XDG_DATA_HOME/dicta`. |
| `--audit-retention-days` | `0` (forever) | Day-directories older than this are deleted at startup and on Close. |

Files are mode `0600`, directories `0700`. Day-directories have format
`YYYY-MM-DD/`. WAV files use the locked PCM format (D15: 16 kHz mono
int16 LE).

## Examples

### Minimal (Wyoming + type-mode only)

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend wyoming \
    --asr-wyoming-addr tcp://localhost:10300
```

### Full (Wyoming + clip-mode + cleanup + audit)

```ini
[Service]
Environment=DICTA_LLM_KEY=local-llama-no-auth
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend wyoming \
    --asr-wyoming-addr tcp://localhost:10300 \
    --preview-binary %h/.local/bin/dicta-preview \
    --cleanup-enabled \
    --cleanup-endpoint http://strix-halo.lan:8080/v1 \
    --cleanup-model qwen3-7b-instruct \
    --audit-enabled \
    --audit-retention-days 30
```

### Local whisper.cpp + cleanup against the same box

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend whispercpp \
    --whispercpp-binary /usr/local/bin/whisper-server \
    --whispercpp-model %h/.local/share/dicta/ggml-base.en.bin \
    --preview-binary %h/.local/bin/dicta-preview \
    --cleanup-enabled \
    --cleanup-endpoint http://localhost:8080/v1 \
    --cleanup-model qwen3-7b-instruct
```

### Hosted OpenAI (transcription + cleanup)

```ini
[Service]
Environment=OPENAI_API_KEY=sk-...
Environment=DICTA_LLM_KEY=sk-...
ExecStart=
ExecStart=%h/.local/bin/dictad \
    --asr-backend openai \
    --asr-openai-key-env OPENAI_API_KEY \
    --preview-binary %h/.local/bin/dicta-preview \
    --cleanup-enabled \
    --cleanup-endpoint https://api.openai.com/v1 \
    --cleanup-api-key-env DICTA_LLM_KEY \
    --cleanup-model gpt-4o-mini
```

## Verifying configuration

```sh
dicta status
```

Returns the current ASR backend, audio capture state, mode (type/clip/
none), session open/closed, and last transcription summary. No
transcript text — `dicta status` is safe to run while screen-sharing.

## Logging

Structured JSON to stdout (under systemd) or text to stderr (TTY).
Levels DEBUG, INFO, WARN, ERROR (default INFO). Tail with:

```sh
journalctl --user -u dictad.service -f
```

Look for these lines:

- `cleanup TLS certificate verification is DISABLED` — you're running
  `--cleanup-tls-skip-verify`. Bad outside testing.
- `audit enabled — per-utterance transcripts will be written to disk`
  — you're running `--audit-enabled`. Make sure you meant to.
- `cleanup endpoint uses http:// — transcript content will be sent in
  cleartext` — you're sending dictation in cleartext. Use https.
