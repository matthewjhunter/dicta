# Contributing

Open an issue before sending a PR for substantial changes — easier to
agree on the approach up front than to rework it in review. See
[dicta-design.md](dicta-design.md) for the design spec; §13 lists the
remaining open decision points and everything else is intentionally
locked (do not change a locked decision without filing an issue first).

## Local development

```sh
task             # list tasks
task build       # build dictad + dicta (pure Go)
task build:all   # build all three (preview needs CGo + Wayland headers)
task test        # go test ./...
task test:race   # go test -race ./... (requires CGo)
task check       # vet + fmt + lint + test + test:race + vuln
```

`task check` must pass before a PR will be merged. Requires Go 1.25.10
or newer.

First-time build dependencies (preview panel only):

```sh
sudo ./scripts/install-deps-ubuntu.sh   # or install-deps-fedora.sh / install-deps-arch.sh
```

## Conventions

- **Pure Go for dictad and dicta (D13).** Both build with
  `CGO_ENABLED=0`. The race detector requires CGo, so `task test:race`
  re-enables it; the production binaries do not. Don't add CGo to the
  daemon or CLI — the systemd unit's `MemoryDenyWriteExecute=true`
  relies on the static-Go invariant.
- **`dicta-preview` is the only CGo carve-out.** Separate process,
  separate boundary; the daemon never embeds Gio.
- **Process topology is load-bearing.** `dictad` owns audio, ASR,
  cleanup, dispatch, and the control socket. `dicta` is the CLI;
  `dicta-preview` is the clip-mode panel. Neither imports
  `internal/`. Adding a new privileged capability means it lives in
  dictad, not in the helpers.
- **No PTT, no wakeword.** v1 ships exactly two compositor bindings
  (D5/D17). Push-to-talk and wakeword detection are out of scope.
- **Run `task fmt:fix` before committing.**

## Adding an ASR backend

asrclient is the home for protocol clients. Adding a new backend means:

1. Add a sub-package in
   [matthewjhunter/asrclient](https://github.com/matthewjhunter/asrclient)
   that implements `asrclient.Transcriber`.
2. Tag a new asrclient release; bump dicta's `go.mod`.
3. Add a `selectFoo` case in `internal/asr/selector.go` and a config
   struct in `internal/asr/config.go`.
4. Add a `--asr-backend foo` flag value and the per-backend flags in
   `cmd/dictad/main.go`.
5. Document the new backend in `README.md` and `CONFIGURATION.md`.

Subprocess lifecycle (port discovery, restart-on-crash) stays in
dicta, not in asrclient — see D16 in the design doc.

## Testing

- Goroutine leak detection: many packages use `TestMain` with goleak.
  New goroutine-heavy code should be added to that pattern.
- The control-protocol parser is fuzzed:
  `go test -fuzz=FuzzCommandUnmarshal -fuzztime=1m ./internal/control`.
- VAD calibration regressions: please include test coverage when
  changing calibration thresholds. The 500ms calibration / 6 dB margin
  / 800ms hangover defaults (§5.1) are load-bearing for the
  hallucination filter.

## Reporting issues

Bugs and feature requests: open a [GitHub issue][issues].

Security vulnerabilities: see [SECURITY.md](SECURITY.md). Don't open
public issues for security problems.

[issues]: https://github.com/matthewjhunter/dicta/issues
