# dicta -- Pluggable Mute Detection (`MuteSource`)

**Status:** v0.1 design, pre-implementation
**Target implementer:** Claude Code
**Owner:** matthewjhunter
**Supersedes / extends:** `dicta-design.md` (no D-row exists yet for mute detection — see §3 below for proposed D18).

---

## 1. Background

The `--unmute-to-dictate` feature added in `4caf729` opens a type-mode
session when the configured mic transitions muted→unmuted and closes it
on the reverse. The current implementation in
`cmd/dictad/mutewatch.go` infers mute state by checking each captured
80 ms PCM frame for all-zero bytes — verified against the **MXL AC-44
TAP**, which gates audio in device firmware and emits literal `0x00`
samples while muted.

This signal is unambiguous on the AC-44 (muted = 100% zero, unmuted-
silent = ~95% nonzero at -69 dBFS noise floor) but the design is
hardware-specific. Mics that mute by attenuating to a real noise floor,
or that expose mute through a host-visible channel instead of in the
PCM stream, will read as "always unmuted" — the watcher never fires.
`CONFIGURATION.md` already warns operators on other hardware to verify
with a `pw-record` probe before enabling the flag.

Empirically confirmed: the AC-44 does **not** expose UAC mute through
PipeWire's `route.mute` property. The PCM-zero side channel is the
only signal that surfaces this mic's hardware mute state. Other mics
likely do expose it; this document specifies a pluggable detection
layer so additional mics can be supported without further hardcoding
in the audio pump.

### 1.1 The two-mic UX pattern

A motivating use case worth keeping in mind: a user with both a
chat/comms headset (e.g. a gaming headset for Discord, Mumble, Teams)
and the AC-44 wired in parallel. The AC-44 lives at the desk with
nothing routed to it by default; its touch-mute button is effectively
a **dedicated dictation toggle** that doesn't compete with the
system's default capture device. Dicta runs on the AC-44 with
`--unmute-to-dictate`; the chat headset stays the PipeWire default and
the user's other software (browser, voice-chat clients) is unaffected.
This pattern only works if `--audio-device` is honored
independently of the system default source, which it already is in
the existing capture path — the new pipewire source must preserve
that property.

## 2. Goals and non-goals

### Goals

- A single `MuteSource` interface that the existing transition logic
  (debounce, clip-mode safety, fire-on-real-transitions) consumes
  without knowing how mute state is determined.
- Two implementations shipped together:
  1. **`pcm-zero`** — current behavior, refactored behind the
     interface. Stays the default for back-compat with AC-44 users
     who already enabled `--unmute-to-dictate`.
  2. **`pipewire`** — new. Watches the source node's `mute` /
     `route.mute` property for changes. Covers the USB Audio Class
     mute control unit, which most prosumer mics expose.
- Pure Go for the daemon (D13). Subprocess transports are acceptable
  per D1/D14 precedent.
- Explicit source selection via a flag. No silent auto-detect — the
  failure mode of auto-detect ("nothing fired in N seconds, but did
  the user press the button?") is indistinguishable from a broken
  backend.
- A `dicta probe-mute` subcommand that runs all available sources in
  parallel for a fixed window, reports what each saw, and recommends
  which to use. Bridges the "verify your hardware first" warning into
  something the user can actually run.

### Non-goals

- **evdev / HID mute detection.** Higher plumbing cost (`/dev/input/eventN`
  permissions, udev matching, parsing `KEY_MICMUTE`/`KEY_MUTE` from
  the input subsystem) for a smaller coverage gain (mostly enterprise
  headsets). Tracked as a follow-up; not in this feature's v1.
- **Mute control.** `dicta` only observes mute state. Setting mute
  from the daemon is out of scope and would invite UX collisions
  with whatever the user clicks in PipeWire/PulseAudio UIs.
- **Auto-detection across sources.** See above — explicit-flag plus
  `probe-mute` is the chosen UX.
- **D-Bus / wireplumber as the PipeWire transport.** More dependencies,
  more surface. The native PipeWire CLI tools (`pw-mon`, `pw-cli`,
  `pw-dump`) are already on every Wayland install where dicta runs.

## 3. Security posture: default-off is mandatory

**`--unmute-to-dictate` is off by default and must stay that way.**
Continuous listening + automatic type-mode activation is not a posture
to put a new user in without an explicit opt-in gesture. The feature
remaining default-off is the primary safety control on this whole
detection layer — every other design choice in this document assumes
that gate is in place.

Concretely:

- `--unmute-to-dictate=false` is the default. Nothing in this design
  changes that.
- `--audio-monitor=false` is also the default — the audio pump itself
  doesn't run without explicit opt-in. The mute feature inherits this
  for free for the `pcm-zero` source; the `pipewire` source does
  *not* depend on the audio pump (it talks to PipeWire directly), so
  enabling the mute feature with `pipewire` still implies "dicta is
  watching mic state continuously" — same posture as before, but
  worth being explicit about.
- The probe subcommand (`dicta probe-mute`, §7.4) is a one-shot
  diagnostic that exits after its window. It does not enable the
  feature; it just tells the user what would happen if they did.

If the default-off posture were ever to change, the rest of this
design would need to be re-examined under a different threat model.
Automatic activation of keystroke synthesis from "someone toggled a
mute button somewhere" is a much riskier default than "someone
explicitly opened a session by pressing Pause."

### 3.1 Proposed locked decision

| ID  | Decision | Rationale |
|-----|----------|-----------|
| D18 | Mute detection for `--unmute-to-dictate` is pluggable via the `mute.Source` interface in `internal/mute`. **The feature itself remains default-off (§3); D18 governs only what happens when the user explicitly opts in.** Two implementations ship in v1 of the feature: `pcm-zero` (current all-zero detector) and `pipewire` (watches the source node's mute property via `pw-mon` subprocess). Default `--unmute-source` is `auto`: both sources are started concurrently and the first to observe a transition wins; the watcher then locks to that source for the rest of the process lifetime. Users may pin to a specific source via `--unmute-source=pcm-zero\|pipewire`. A `dicta probe-mute` subcommand runs all sources in parallel for a fixed window and prints what each saw — diagnostic only, not required for normal use. | Decouples the AC-44 quirk from the watcher; broadens supported hardware to anything that surfaces mute through the UAC mute control unit. Subprocess transport matches D1/D14 (pw-record/parec already in tree). Autodetect-on-enable is safe because the enable gesture is itself the user decision — no ambiguity to resolve at probe time. |

## 4. Architecture

### 4.1 Package layout

```
internal/mute/
├── doc.go                  package overview
├── source.go               interface, Event, State, Name constants
├── source_test.go          interface-level tests (fake source)
├── pcmzero/
│   ├── source.go           refactored all-zero PCM detector
│   └── source_test.go      (moves the meat of mutewatch_test.go here)
└── pipewire/
    ├── source.go           pw-mon subprocess + change-event parser
    ├── source_test.go      parser tests against captured pw-mon output
    └── monitor.go          pw-mon process lifecycle helper
```

`cmd/dictad/mutewatch.go` keeps the transition-firing logic (debounce,
clip-mode safety, EnsureTypeOpen / CloseIfTypeOpen) but consumes a
`mute.Source` instead of receiving raw PCM frames. The existing
`audioMonitor.onFrame` hook stays — but only the pcm-zero source
subscribes to it; pipewire is independent of the audio pump.

### 4.2 Wiring

```
                    ┌──────────────────────────┐
                    │ mute.Source (selected)   │
                    │  - pcmzero (reads        │
                    │    onFrame from          │
                    │    audioMonitor)         │
                    │  - pipewire (reads       │
                    │    pw-mon subprocess)    │
                    └────────────┬─────────────┘
                                 │ Event chan
                                 ▼
                    ┌──────────────────────────┐
                    │ muteWatcher              │
                    │  - debounce              │
                    │  - clip-mode safety      │
                    │  - fire on transition    │
                    └────────────┬─────────────┘
                                 │
                                 ▼
                    ┌──────────────────────────┐
                    │ session                  │
                    │  EnsureTypeOpen          │
                    │  CloseIfTypeOpen         │
                    └──────────────────────────┘
```

## 5. The `mute.Source` interface

```go
// Package mute provides pluggable hardware-mute detection for
// dicta's --unmute-to-dictate watcher. Implementations observe the
// configured microphone and emit State transitions to a channel that
// the watcher consumes. Sources are independent of how they sample
// mute state — PCM-zero detection, PipeWire property change, evdev
// event, vendor HID, etc. all conform to the same Source contract.
package mute

import (
    "context"
    "time"
)

// State is the observed mute state of a microphone. Unknown is the
// startup value before any source has reported.
type State int

const (
    Unknown State = iota
    Unmuted
    Muted
)

func (s State) String() string {
    switch s {
    case Unmuted:
        return "unmuted"
    case Muted:
        return "muted"
    default:
        return "unknown"
    }
}

// Event is a single mute-state observation from a Source.
type Event struct {
    // State is the observed mute state at At.
    State State

    // At is the wall-clock time the source observed this state. Used
    // by the watcher's debounce logic and audit logging only — not
    // for ordering (the channel is already ordered).
    At time.Time

    // Source is the Name() of the source that produced this event,
    // copied here so log handlers don't need to keep a reference to
    // the producing Source. Useful when probe-mute fans multiple
    // sources into a single log.
    Source string

    // Initial is true for the first event a Source emits after Watch
    // returns. The watcher must NOT treat an Initial event as a
    // transition — it only seeds lastState. This matches the existing
    // pcm-zero behavior of "first frame seeds lastMuted; the user
    // has to do a real mute/unmute action to invoke the watcher."
    Initial bool
}

// Source observes the mute state of a single microphone.
//
// Implementations must:
//   - Return events in observation order on the channel returned by
//     Watch. The first event has Initial=true and represents the
//     state as observed at startup (or Unknown if no observation is
//     possible without a transition).
//   - Close the returned channel before the goroutine that produced
//     events exits, exactly once, after ctx is cancelled or an
//     unrecoverable error occurs.
//   - Be safe to call Watch only once per instance. Concurrent Watch
//     calls on the same Source are not supported.
//
// Implementations should:
//   - Filter out no-op events (consecutive identical states) so the
//     watcher's debounce counter only ticks on real changes.
//   - Coalesce bursty changes if the underlying transport is noisier
//     than the user-visible mute action (e.g. PipeWire emitting two
//     property updates for a single button press).
type Source interface {
    // Name is a short, stable identifier for logs, status output,
    // and the --unmute-source flag value. Lowercase, no spaces.
    // Examples: "pcm-zero", "pipewire", "evdev".
    Name() string

    // Describe returns a one-line human description of what this
    // source watches. Shown by `dicta probe-mute` and in startup
    // logs. Example: "PipeWire route.mute on source node 42".
    Describe() string

    // Watch starts observing mute state and returns a channel of
    // events. The channel is closed when ctx is cancelled or the
    // source hits a terminal error. An error returned here means
    // the source could not start; per-event errors are surfaced via
    // logging inside the implementation (the channel keeps streaming
    // observations the source CAN make).
    Watch(ctx context.Context) (<-chan Event, error)
}

// MultiSource is the variant probe-mute uses to fan several sources
// into a single channel for side-by-side comparison. Not used by the
// production watcher, which only consumes one configured Source.
type MultiSource interface {
    Source
    Subsources() []Source
}
```

### 5.1 Interface notes

- **`Initial` flag carries the seed-vs-transition distinction.** The
  current watcher uses `started bool` to suppress firing on the first
  frame. Moving this responsibility into the source's event stream
  (rather than into the watcher) means a source that genuinely
  observes a transition at startup (e.g. PipeWire reporting the
  current mute state in its initial enumeration before any change
  arrives) can flag it `Initial=true` and the watcher does the right
  thing without source-specific knowledge.

- **No `Ping()` method.** The interface deliberately does not
  expose a health probe. If a source can't start, `Watch` returns
  an error. If a source dies mid-stream, it closes the channel and
  the watcher logs and gives up — there's no graceful re-attach
  path in this feature's v1. (Reconnect-on-fail is a follow-up if
  pipewire turns out to flap in practice.)

- **Buffered channel, depth 1.** Watch's implementation returns a
  buffered channel (depth 1) so a source that briefly outpaces the
  watcher doesn't block its observation goroutine. Drop policy:
  newest wins, oldest is overwritten. State is idempotent — losing
  an intermediate observation is fine as long as the final state
  arrives.

## 6. Source implementations

### 6.1 `pcmzero.Source`

Refactor of the current `muteWatcher.OnFrame` + `isAllZero` logic.

- **Construction:** takes a frame stream (the existing
  `audioMonitor.onFrame` callback registration point). The source
  attaches itself as the `onFrame` handler when `Watch` is called.
- **State derivation:** identical to current — `isAllZero(pcm)` per
  frame, no threshold.
- **First-event handling:** the first frame produces an `Initial=true`
  event reflecting whatever state was observed.
- **Drop:** the embedded debounce in `muteWatcher.counter` moves to
  the watcher, where it lives now. The source emits raw observations.

Wait — that means the source emits one event per frame (12.5/s at
80 ms frames), even though most are duplicates. To avoid that, the
source coalesces consecutive identical observations and only emits
when the state changes (per §5's "filter out no-op events"). The
watcher then handles temporal debounce on top.

### 6.2 `pipewire.Source`

Spawns `pw-mon` as a long-lived subprocess and parses its stream.

**Transport choice rationale.** Three candidates considered:

1. `pw-mon` (subprocess, parse event stream). Picked.
2. `pw-cli enum-params <NODE> Props` polled every 80 ms. Rejected:
   fork/exec per poll has measurable cost, and 80 ms latency is
   already worse than what `pw-mon` gives streaming.
3. Native PipeWire protocol over `$XDG_RUNTIME_DIR/pipewire-0`.
   Rejected for v1: requires implementing the SPA pod format
   (variable-length-encoded binary protocol) — substantial new
   code surface for a feature where subprocess works fine. Kept
   open as a v2 escape hatch if `pw-mon` proves unstable.

**Device matching.** The source is constructed with the same device
identifier passed to `--audio-device`. On `Watch`, it enumerates
PipeWire nodes via `pw-dump`, finds the source node matching the
device, and remembers its node ID. Subsequent `pw-mon` events are
filtered by that ID.

**Initial state.** The dump-then-watch sequence gives a definite
initial state to emit as `Initial=true` — no Unknown placeholder
needed for this source.

**Mute property name.** Depends on PipeWire version and the device's
Param topology. The implementation checks both `mute` (top-level
Props) and `route.mute` (nested under Route info) and accepts a
change to either. Tests cover both shapes against captured
`pw-mon` output.

**Edge case: device hot-unplug.** If the matched node disappears
mid-stream (USB mic unplugged), the source logs and closes its
channel — same as a terminal error. The watcher gives up; if the
user replugs and wants the feature back, they restart dictad. (A
re-attach implementation is a follow-up.)

## 7. Configuration

### 7.1 New flags

```
--unmute-source=auto|pcm-zero|pipewire   (default: auto)
    Detection backend for --unmute-to-dictate. `auto` (default,
    recommended) starts all available sources concurrently and locks
    to the first one that observes a real transition — this Just
    Works across the supported hardware in §8.2 with no per-mic
    configuration. Pin to a specific source if you've tested with
    `dicta probe-mute` and want deterministic behavior:
      - pcm-zero detects mute via all-zero PCM frames (works on MXL
        AC-44 and similar; requires --audio-monitor).
      - pipewire watches the source node's mute property (works on
        mics that surface mute through the UAC mute control unit).
```

The existing `--unmute-to-dictate` and `--unmute-to-dictate-debounce`
flags are unchanged. `--audio-monitor` remains required only when the
selected source needs the audio pump — that is, when `pcm-zero` is
either explicitly selected or is one of the candidates in `auto`
mode. In practice this means `--audio-monitor` should be passed
alongside `--unmute-to-dictate` for the common case; we can revisit
making it implicit in a follow-up.

### 7.2 Default rationale

The `--unmute-to-dictate` feature itself remains off by default — see
§3. When the user explicitly enables it, `auto` is the default source
because it requires no per-mic knowledge to be useful: the user
plugs in their mic, enables the feature, presses the mute button, and
whichever source happens to fire wins. Auto-detect is safe here only
because the enabling gesture is the explicit opt-in; this is **not** a
pattern to generalize to other features without re-examining the
threat model (see §3).

Pinning to `pcm-zero` or `pipewire` is available for users who want
deterministic behavior — for example, an AC-44 user who has confirmed
via `dicta probe-mute` that only `pcm-zero` works for them and would
rather not spawn `pw-mon` for no reason. The pinned modes are also
useful when reporting bugs ("`auto` flapped between sources" is
harder to diagnose than "`pipewire` missed a transition").

In `auto` mode, source failure during startup is non-fatal: if one
source can't start (e.g. `pw-mon` not in PATH), the daemon logs at
WARN and continues with whichever sources did start. If all sources
fail to start, the daemon logs at ERROR and `--unmute-to-dictate`
silently no-ops for the process lifetime — the daemon stays up, and
Pause-key activation continues to work.

### 7.3 Device targeting

Both sources accept the same device identifier as dictad's existing
`--audio-device` flag:

- **Explicit `--audio-device=<name>`**: the source watches exactly
  that mic. Required when the user wants dicta on a non-default mic
  (the two-mic UX in §1.1).
- **Empty `--audio-device`**: the source falls back to the system
  default capture device. For `pcm-zero` this means the device
  `pw-record` would pick with no args; for `pipewire`, the source
  node currently flagged as the default in `pw-dump`. The default
  can move (PipeWire's session manager may reassign on hot-plug); the
  source re-resolves on each `Watch` call but does **not** follow
  defaults that change mid-session. A default change during a session
  triggers a close-and-log; the watcher gives up until restart. (A
  follow-up may add follow-the-default behavior; not in v1.)

For `dicta probe-mute`, the same rule applies — `--device` if given,
system default otherwise.

### 7.4 `dicta probe-mute` subcommand (diagnostic)

`probe-mute` is **not** required to use the feature — `auto` mode
(§7.2) handles source selection at runtime. The subcommand exists
for two cases:

1. **Bug reporting.** "Which sources see anything on my mic?" is the
   first question to answer when `--unmute-to-dictate` behaves
   unexpectedly. Probe output gives a clean before-the-watcher view
   of source behavior.
2. **Pinning decisions.** A user who wants to pin
   `--unmute-source=pcm-zero` or `pipewire` for the reasons in §7.2
   can run probe-mute to confirm their pick works.

```
$ dicta probe-mute --device alsa_input.usb-MXL_AC-44 --seconds 15
probe-mute: running 2 sources for 15s on alsa_input.usb-MXL_AC-44
probe-mute: please toggle the mute button on your mic now

[0.04s] pcm-zero: initial=unmuted
[2.31s] pcm-zero: transition unmuted→muted
[2.31s] pipewire: (no events yet)
[5.10s] pcm-zero: transition muted→unmuted
[5.10s] pipewire: (no events yet)

probe-mute: 15s elapsed

results:
  pcm-zero  : 1 initial + 2 transitions      ✓ recommended
  pipewire  : 0 initial + 0 transitions      ✗ no signal

recommendation: --unmute-source=pcm-zero
```

Probe writes to stdout, exits 0 if at least one source detected
transitions, exits 1 if none did. Useful for both interactive
verification and CI-style hardware testing.

## 8. Testing plan

### 8.1 Unit tests

- `internal/mute`: interface compliance with a `fakeSource`, channel
  closure on ctx cancel, no leak under `goleak.TestMain`.
- `internal/mute/pcmzero`: refactored versions of the existing
  `mutewatch_test.go` tests. Same coverage (zero/nonzero/transition/
  debounce-suppression) but exercised through the Source contract.
- `internal/mute/pipewire`: parser tests against captured `pw-mon`
  output (committed as testdata). Cover both `mute` and `route.mute`
  property shapes, hot-unplug, device-not-found.

### 8.2 Hardware test matrix

Once the implementation lands, run `dicta probe-mute --seconds 15`
against every USB mic in the lab. Currently-attached inventory (per
`lsusb` on the dev workstation, 2026-05-14):

| Mic | USB ID | PipeWire node | pcm-zero | pipewire | Notes |
|-----|--------|---------------|----------|----------|-------|
| MXL AC-44 TAP | 15dd:0010 | `alsa_input.usb-MXL_MXL_AC-44_TAP-00.mono-fallback` | ✓ (confirmed) | ✗ (confirmed — no UAC mute) | Baseline. Touch-mute button gates audio in firmware; only the PCM-zero side channel surfaces state. |
| SteelSeries Arctis Pro Wireless | 1038:1290 + 1038:1294 | `alsa_input.usb-SteelSeries_Arctis_Pro_Wireless-00.mono-chat` | ? | ? | Wireless headset with a mute button on the cup. Unknown whether button flips UAC mute or only a vendor HID channel. |
| Sennheiser/EPOS GSP 370 | 1395:009a (DSEA A/S) | (capture node — confirm during probe) | ? | ? | Boom-arm-down mute. Typical EPOS pattern is HID Telephony, which would mean neither pcm-zero nor pipewire fires; evdev follow-up would cover it. |
| Blue Yeti (older) | 0b0e:* | — | ? | ? | User reports two units buried somewhere — surface for the test pass. Older firmware flipped the ALSA capture switch on button press, which pipewire `route.mute` should reflect. |
| (motherboard analog) | — | `alsa_input.pci-0000_c3_00.6.analog-stereo` | ? | ? | Useful as a "host-controlled mute only" baseline since there's no physical button — driven entirely by ALSA capture switch. |

Empty cells get filled in during the test pass. Results get appended
to a `mute-source-matrix.md` so future users can look up known-good
configurations.

If the matrix shows two or more mics in the lab needing HID/evdev
detection (likely for the GSP 370 specifically), that elevates evdev
from "follow-up" to "next feature increment" — note in the matrix
write-up.

### 8.3 Regression gate

`go test ./internal/mute/... -race` plus the existing
`cmd/dictad/...` tests under `task check`. No new integration test
infrastructure required.

## 9. Security considerations

The pipewire source adds a `pw-mon` subprocess to the daemon's
runtime topology. Per the SECURITY.md §8 map this counts as a new
spawned process and should be listed there with its lifecycle and
exit handling. No new permissions: `pw-mon` runs as the same user as
dictad and only needs access to the existing PipeWire user socket
(`$XDG_RUNTIME_DIR/pipewire-0`) that `pw-record` already uses.

No new sensitive data crosses a process boundary. `pw-mon` output
contains node names and parameter values (mute state), not audio
samples or transcripts.

## 10. Migration

- The current `cmd/dictad/mutewatch.go` and `mutewatch_test.go` move
  to `internal/mute/pcmzero/` with minimal logic changes. The
  watcher (transition firing, debounce, clip-mode safety) stays at
  `cmd/dictad/mutewatch.go` but rewires to consume `mute.Event`
  channel events rather than PCM frames.
- `cmd/dictad/main.go` gains the new flag and the source-selection
  switch. `audioMonitor.onFrame` is set only when
  `--unmute-source=pcm-zero` is selected.
- No config file changes. No new systemd unit changes. No README
  changes beyond a one-paragraph note pointing users at
  `dicta probe-mute`.
- `CONFIGURATION.md` §"Unmute-to-dictate" expands with the new flag
  documentation and a short table of the two sources.

## 11. Open questions

All resolved as of 2026-05-14:

1. ~~Does the AC-44 also expose UAC mute via `route.mute`?~~ No
   (verified). PCM-zero stays the only working source for this mic.
   Default for `--unmute-source` remains `pcm-zero`.
2. ~~What other USB mics for the test matrix?~~ Inventoried in §8.2:
   AC-44, SteelSeries Arctis Pro Wireless, Sennheiser/EPOS GSP 370,
   two older Blue Yetis to be surfaced, and the motherboard analog
   input as a host-controlled-mute baseline.
3. ~~`dicta probe-mute` device targeting?~~ §7.3: explicit `--device`
   wins; absent that, system default capture device. Same rule for
   dictad's `--audio-device`. Two-mic pattern (§1.1) is the
   motivating case.
