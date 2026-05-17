# Security posture

This document maps the §8 security model from `dicta-design.md` to the
specific code, tests, and configuration that enforce each item. The
design doc is the spec; this file is the audit map.

If you are reviewing dicta's security, start here. Each item names the
concrete enforcement point so you can verify the claim.

## Reporting

Vulnerabilities: report privately via [GitHub's private vulnerability
reporting][gh-pvr] for this repository. As a fallback, you can email
matthewjayhunter@gmail.com. Do not file public issues for exploitable
bugs.

[gh-pvr]: https://github.com/matthewjhunter/dicta/security/advisories/new

## Threat model summary

dicta is a per-user Linux daemon with two privileged capabilities:
synthesizing keystrokes (via `ydotoold`) and capturing microphone
audio (via PipeWire/PulseAudio). It runs as the user — not root, not
in the `input` group — and ships single-key activation only (no PTT,
no kernel evdev access). The threat we mitigate is "untrusted-but-not-
privileged code on the same machine attempts to misuse dicta's IPC,
its TLS posture, or its filesystem footprint." Threats we do NOT
defend against: a fully-compromised user account, a compromised
PipeWire daemon, or a malicious browser extension snooping the same
microphone stream.

## Boundary checklist

### 1. Input synthesis is privileged (D5, §8.1)

Type-mode is the only path that can synthesize keystrokes; clip-mode
goes through `wl-copy` and the user pastes manually. Enforcement:

- `cmd/dictad/session.go` — `OnUtterance` only invokes `Type()` when
  `mode == modeType`. The Pause toggle is the user's input gate.
- `internal/dispatch/typer.go` — `\n` is stripped from every
  payload before invoking `ydotool` (D12). Tests in
  `internal/dispatch/typer_test.go` cover the strip behavior.
- The dispatch package validates the `ydotool` binary path against
  an allowlist (`pathOnAllowlist`); off-allowlist binaries fail at
  daemon construction, not at first dispatch.

### 2. Audio capture is on-demand (§8.2)

Capture starts when a session opens and stops when it closes. There is
no always-on listening in v1 (wakeword is deferred to v2 per D3).

- `cmd/dictad/audiomonitor.go` — capture is started/stopped by the
  daemon, with the session orchestrator deciding when via `Toggle`.
- `cmd/dictad/session.go::Shutdown` — explicitly closes any open
  session on SIGTERM so capture stops cleanly.

### 3. LLM cleanup endpoint can leak transcripts (§8.3)

Cleanup ships **disabled by default**. Enabling requires explicit
`--cleanup-enabled` plus an endpoint URL.

- `internal/cleanup/cleaner.go::New` — returns the passthrough
  cleaner unless `Enabled=true`.
- `internal/cleanup/http.go` — emits a startup WARN if the endpoint
  is `http://` (transcripts in cleartext) or if
  `tls_verify=false`.
- The mechanical system prompt is the constant
  `MechanicalSystemPrompt`; tests in `cleaner_test.go` enforce that
  it is not runtime-templated (no `%s`/`%d`).

### 4. Clip-mode never types (§8.4)

All clip-mode output goes to `wl-copy`; the panel buffer is the safety
boundary (D12). Nothing reaches the clipboard until the user presses
Enter inside the panel.

- `cmd/dictad/session.go::Commit` — the only path that calls
  `clipper.Clip`, gated on `mode == modeClip`. Cancel discards the
  buffer.
- `internal/dispatch/clipper.go` — pipes via stdin (no argv);
  `wl-copy` binary path validated against the allowlist.
- `cmd/dicta-preview/main.go` — Enter and Escape are scoped via
  `key.Filter` so they reach the panel logic; nothing else triggers
  commit.

### 5. Whisper supply chain (§8.5)

dicta does not auto-download models or whisper-server binaries. Users
install both themselves. Enforcement:

- `internal/whispersup/config.go` — `Binary` and `ModelPath` are
  validated against allowlists (`DefaultBinaryAllowlist`,
  `DefaultModelPathAllowlist`).
- `internal/whispersup/supervisor.go` — `exec.Command` is built from
  typed config values, never via shell.

### 6. Pure-Go daemon (D13, §8.6)

`dictad` and `dicta` build with `CGO_ENABLED=0`. `MemoryDenyWriteExecute=true`
in the systemd unit relies on this property — anything that introduces
CGo or a JIT into the daemon will cause the kernel to refuse the page
mappings and the daemon will fail to start.

- `packaging/systemd/dictad.service` — `MemoryDenyWriteExecute=true`.
- `cmd/dicta-preview/` is the **only** binary permitted to use CGo
  (Gio Wayland), and it runs as a separate user process without the
  daemon's hardening.

### 7. TOML config, no shell-out (§8.7)

- `internal/config/` — typed TOML parser (per design; phase 9 of the
  build).
- Subprocess argv lists are built from typed config values everywhere:
  `internal/dispatch/typer.go`, `internal/dispatch/clipper.go`,
  `internal/whispersup/supervisor.go`, `cmd/dictad/preview.go`.
- Path values are allowlist-checked (see items 1, 5, 11).

### 8. Socket auth via filesystem permissions (§8.8)

The control socket is created with mode `0600`, user-owned. Adequate
for single-user systems; no cryptographic auth.

- `internal/control/server.go::Listen` — `os.Chmod(addr, 0o600)`
  immediately after Listen.
- `proto/proto.go::MaxLineBytes = 64 * 1024` — bounds the per-line
  parse work to mitigate DoS via huge JSON.
- `internal/control/fuzz_test.go::FuzzCommandUnmarshal` — verifies the
  parser tolerates malformed/oversized/embedded-NUL input without
  panic.

### 9. TLS verification on by default (§8.9)

All HTTP clients verify certificates. `tls_verify = false` is testing-
only and emits a startup WARN.

- `internal/asr/selector.go::selectOpenAI` — emits a WARN on
  `InsecureSkipTLSVerify=true`. Test:
  `TestSelectOpenAI_InsecureSkipVerifyEmitsWarning`.
- `internal/cleanup/http.go::newHTTPCleaner` — same WARN. Test:
  `TestNew_InsecureSkipVerifyEmitsWarning`.

### 10. Newline injection (D12)

Type-mode dispatch strips `\n` defensively before ydotool to prevent
newline injection into shell prompts.

- `internal/dispatch/typer.go::Type` — strips `\n` and `\r`. Tests in
  `typer_test.go::TestTyper_StripsNewlines`.
- Clip-mode does **not** strip `\n` because Shift+Enter is a documented
  user action in the panel; the clipboard is the boundary.

### 11. Audit privacy (default-off, phase 11)

Transcript and audio capture are sensitive by definition. Both opt-in.

- `internal/audit/audit.go::New` — returns passthrough unless
  `Enabled=true`.
- `--audit-enabled` (JSONL) and `--audit-keep-audio` (WAV) are
  separate flags. Daemon logs a WARN when audit is enabled so
  accidental opt-ins are visible in `journalctl`.
- `internal/audit/audit.go::sanitizeID` — utterance IDs are
  sanitized before becoming filenames so a misbehaving ASR backend
  can't path-traverse out of `audio/`.
- `internal/audit/config.go::DefaultPathAllowlist` + path validation
  in `New` — rejects directories outside `$XDG_DATA_HOME/dicta` or
  `/var/lib/dicta`.
- Files are mode `0600`, directories `0700`.
- Retention sweeper runs at startup and via `SweepLoop`; deletes day-
  directories older than `RetentionDays` (`0` = forever).
- The Record's `PCM` field has `json:"-"` so audio bytes never
  serialize into JSONL.

### 12. systemd hardening (§7.1)

`packaging/systemd/dictad.service` applies the full hardening posture:

- `NoNewPrivileges=true`
- `ProtectSystem=strict`, `ProtectHome=read-only`,
  `ReadWritePaths=%h/.local/share/dicta %t`
- `PrivateTmp=true`
- `ProtectKernelTunables=true`, `ProtectKernelModules=true`,
  `ProtectControlGroups=true`
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` — no netlink,
  no packet sockets.
- `RestrictNamespaces=true`, `LockPersonality=true`,
  `MemoryDenyWriteExecute=true` (relies on D13).
- `RestrictRealtime=true`
- `SystemCallFilter=@system-service`,
  `SystemCallErrorNumber=EPERM`

## Non-goals

dicta does **not** attempt to defend against:

- A fully-compromised user account.
- A compromised PipeWire/PulseAudio daemon (it would have direct
  microphone access regardless).
- A malicious systemd unit running as the same user (it could read
  the control socket directly).
- Side-channel attacks against the cleanup or ASR endpoints.
- Network adversaries when the user explicitly opts into
  `tls_verify = false` (the WARN at startup is the contract).

## Test coverage

- `go test -race ./...` — race-detector clean across the repo.
- `goleak` is wired into TestMain for every package with goroutines
  (`cmd/dictad`, `internal/audio`, `internal/asr`, `internal/audit`,
  `internal/cleanup`, `internal/control`, `internal/whispersup`).
- `go test -fuzz=FuzzCommandUnmarshal ./internal/control` exercises the
  wire-protocol parser; the seed corpus runs as a regular test on
  every CI build.
