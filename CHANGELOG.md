# Changelog

All notable changes to dicta are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries describe what changed for someone running or building dicta. The complete development record lives in the git history.

## [Unreleased]

### Added

- `dicta check` runs a real end-to-end ASR check: it submits an embedded "Hello world" fixture to the configured backend and compares the returned transcript. A misconfigured or unreachable backend is now diagnosable without dictating into it and guessing. This is a live round trip, so it takes seconds.
- `dicta suspend` and `dicta resume` turn the `--unmute-to-dictate` watcher off and back on without restarting the daemon. `dicta status` reports the watcher's state as `auto_activation`.
- A flap guard for `--unmute-to-dictate`. If more than `--unmute-flap-threshold` mute transitions fire within `--unmute-flap-window` (default: 6 within 10 seconds), the watcher suspends itself, so a noise-gated non-dictation device cannot loop the mic-cue tone indefinitely. `dicta resume` clears it; `--unmute-flap-threshold 0` disables the guard.
- `dictad` signals readiness to systemd with `sd_notify(READY=1)` once the control socket is bound and accepting.

### Changed

- **Building dicta now requires Go 1.26.8 or newer.** Earlier toolchains carry standard-library vulnerabilities in the `crypto/tls`, `crypto/x509`, `net/http`, `net/url` and `encoding/asn1` paths that the cleanup and OpenAI ASR clients reach; the worst of them are not fixed until 1.26.6.
- **The packaged systemd unit is back to `Type=notify`.** If you update `packaging/systemd/dictad.service`, update the binary at the same time -- a `dictad` built before this release, running under a `Type=notify` unit, never signals readiness and will sit in `activating` until systemd's start timeout. A current binary under an old `Type=simple` unit is fine; the notification is simply skipped.
- **`--audio-monitor` is no longer needed for dictation.** Audio capture is now on-demand: the microphone is opened when a session opens and released when it closes. The flag now only selects continuous capture, which matters for idle VAD statistics in `dicta status` and for the `pcm-zero` and `auto` unmute sources, which have to watch frames between sessions.
- **`dicta status` no longer reports a polled ASR health value.** The daemon previously probed the backend on a timer and published the result; the probe was removed rather than left to report a stale or misleading value. `asr.health` now reads `unchecked` until you run `dicta check`. Anything parsing that field should expect the new value.

### Fixed

- Type-mode worked only with `--audio-monitor` set. Without it the daemon started cleanly, accepted `toggle_talk` over the control socket, and answered `not_implemented`. The Pause key did nothing and the journal said nothing about why.
- The `whispercpp` backend could not transcribe. `dictad` now starts `whisper-server` with `--inference-path`, so the subprocess serves the OpenAI-compatible transcription path its client actually requests.
- Utterances longer than `--vad-max-utterance` were force-split wherever the buffer happened to fill, which cut words in half and garbled the transcript on both sides of the cut. The split now falls at the quietest point within the tail of the buffer, so it lands in a pause.
- An utterance still in flight when a type-mode session closed was dropped. In-flight VAD audio is now flushed and queued transcripts are allowed to drain, so the last phrase before Pause is not lost.
- `dicta-preview` could not be launched under the documented user install. The daemon's preview-binary allowlist rejected `~/.local/bin`, where `task install:user` places it, which left clip-mode (Scroll Lock) unusable without a manual workaround.

## [0.1.2] - 2026-06-27

### Added

- A circuit breaker on the ydotool dispatch path. After 3 consecutive `Type` failures the breaker opens and further calls are shed immediately instead of invoking a wedged `ydotool` and hanging on every utterance. It probes again after a 30 second cooldown and closes on the first success, and notifies once when it opens.

### Changed

- README documents the GNOME keybindings helper, and its em-dashes are plain ASCII.

## [0.1.1] - 2026-06-27

### Changed

- Raised the Go floor to 1.25.11 for the `crypto/x509` standard-library fix in GO-2026-5037.

### Fixed

- Documented the `ydotoold` file-descriptor leak and shipped example units that work around it. `ydotoold` accumulates accept'd client sockets during normal use and wedges against the default `LimitNOFILE=1024`; the shipped unit raises it to 65536 and pairs it with a nightly restart timer. See [packaging/systemd/README.md](packaging/systemd/README.md#ydotoold-fd-leak-workaround). The underlying bug is upstream in ReimuNotMoe/ydotool and is not fixed here.
- Corrected the documented minimum Go version to match `go.mod`, and refreshed repository paths in the docs.

## [0.1.0] - 2026-05-16

First release. A Linux/Wayland-first voice dictation daemon in pure Go.

### Added

- **Type-mode.** Pause toggles a dictation session. While it is open, VAD silence commits each utterance through `ydotool` and the session stays open for the next phrase. Single-line by design: no newline is ever synthesized, and `\n` is stripped defensively before dispatch.
- **Clip-mode.** Scroll Lock toggles the `dicta-preview` panel, a Gio sidecar showing the live transcript in an editable text area. Enter commits to the Wayland clipboard via `wl-copy`, Shift+Enter inserts a literal newline, Esc cancels. Nothing reaches the clipboard until you press Enter.
- **Three ASR backends,** selectable per config: Wyoming over TCP (default), a dicta-supervised local `whisper-server`, and any OpenAI-protocol HTTP endpoint. The wire protocols come from the `asrclient` module; `dictad` adds backend selection and `whisper-server` process lifecycle (spawn, port discovery, health gating, restart on crash).
- **`--unmute-to-dictate`,** off by default: watch the configured microphone's mute state and open or close a type-mode session on the transition, turning the mic's hardware mute button into the toggle. Two pluggable detection backends, `pcm-zero` and `pipewire`, with a compatibility matrix in the docs -- no single method works on every microphone.
- **Optional LLM cleanup** of clip-mode text against an OpenAI-protocol endpoint, off by default. The system prompt is a code constant and is never templated from user input.
- **Control socket** at `$XDG_RUNTIME_DIR/dicta.sock`, mode 0600: newline-delimited JSON, with a command channel and a subscribable event stream. The `dicta` CLI is a thin client over it.
- **Audit logging,** off by default: per-utterance JSONL records, optionally with WAV captures, under `$XDG_DATA_HOME/dicta` with configurable retention. Transcripts are sensitive, so this stays opt-in.
- **Hardened systemd user unit,** a GNOME keybindings installer, dependency install scripts for Ubuntu/Fedora/Arch, and `README`/`CONFIGURATION.md`/`SECURITY.md`.

### Security

- Subprocess argv lists are built from typed config values and never pass through a shell. Binary paths are validated against an allowlist of prefixes, and missing binaries fail at startup rather than mid-commit.
- TLS verification defaults on for every HTTP client. The `tls_verify = false` knobs are testing-only and log a startup warning.
- `MemoryDenyWriteExecute=true` in the unit requires the daemon to be pure Go with no CGo, JIT, or embedded interpreter.
- The control-protocol command parser is fuzzed, and `goleak` guards the goroutine-heavy packages.

[Unreleased]: https://github.com/matthewjhunter/dicta/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/matthewjhunter/dicta/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/matthewjhunter/dicta/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/matthewjhunter/dicta/releases/tag/v0.1.0
